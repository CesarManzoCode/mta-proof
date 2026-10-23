package engine

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/CesarManzoCode/mta-proof/internal/clock"
	"github.com/CesarManzoCode/mta-proof/internal/store"
)

// OpenCore loads the store into a new core and commits start-up recovery
// before returning, so no request is served on top of uncommitted
// recovery events.
func OpenCore(st *store.Store, clk clock.Clock, seed uint64) (*Core, error) {
	snap, err := st.Load()
	if err != nil {
		return nil, err
	}
	c := NewCore(snap, clk, clock.Rand(seed, "engine.core"))
	if err := st.Apply(c.TakeWrites()); err != nil {
		return nil, err
	}
	return c, nil
}

// ServeConfig configures the real-mode engine process.
type ServeConfig struct {
	DB       string // engine.db path
	HTTPAddr string // ingest API address
	Seed     uint64
	MaxBatch int
	Ready    func(net.Addr) // called once the API listens
}

// Serve runs the real-mode engine with the wall clock until ctx is done.
func Serve(ctx context.Context, cfg ServeConfig) error {
	st, err := store.Open(cfg.DB)
	if err != nil {
		return err
	}
	defer st.Close()
	core, err := OpenCore(st, clock.Real{}, cfg.Seed)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	runner := NewRunner(core, st, cfg.MaxBatch)
	runErr := make(chan error, 1)
	go func() { runErr <- runner.Run(ctx) }()
	srv := &http.Server{Handler: NewAPI(runner), ReadHeaderTimeout: 10 * time.Second}
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Serve(ln) }()
	if cfg.Ready != nil {
		cfg.Ready(ln.Addr())
	}
	var result error
	select {
	case <-ctx.Done():
	case result = <-runErr:
	case result = <-srvErr:
	}
	shut, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	srv.Shutdown(shut)
	cancel()
	if errors.Is(result, context.Canceled) || errors.Is(result, http.ErrServerClosed) {
		result = nil
	}
	return result
}
