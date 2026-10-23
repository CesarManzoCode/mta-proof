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

// Serve runs the ingest API alone with the wall clock until ctx is done.
func Serve(ctx context.Context, cfg ServeConfig) error {
	return run(ctx, cfg, nil)
}

// RunConfig configures the real-mode engine with delivery.
type RunConfig struct {
	DB        string
	Config    *Config
	Transport Transport // nil: TCP from the configured source addresses
	Resolver  Resolver  // nil: DNS queries to Config.DNS
	Ready     func(net.Addr)
}

// Run runs the ingest API and delivery with the wall clock until ctx is
// done (mtaengine run).
func Run(ctx context.Context, rc RunConfig) error {
	cfg := rc.Config
	return run(ctx, ServeConfig{DB: rc.DB, HTTPAddr: cfg.HTTP, Seed: cfg.Seed, MaxBatch: cfg.MaxBatch, Ready: rc.Ready}, &rc)
}

func run(ctx context.Context, cfg ServeConfig, rc *RunConfig) error {
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
	if rc != nil {
		core.SetDelivery(rc.Config, clock.Rand(cfg.Seed, "engine.mx"))
	}
	runner := NewRunner(core, st, cfg.MaxBatch)
	runErr := make(chan error, 1)
	go func() { runErr <- runner.Run(ctx) }()
	var onAccept func()
	delErr := make(chan error, 1)
	delivering := rc != nil
	if delivering {
		dc := rc.Config
		tr, res := rc.Transport, rc.Resolver
		if tr == nil {
			tr = &TCPTransport{ConnectTimeout: secondsDur(dc.Timeouts.ConnectS),
				CommandTimeout: secondsDur(dc.Timeouts.CommandS), DataEndTimeout: secondsDur(dc.Timeouts.DataEndS)}
		}
		if res == nil {
			res = &DNSResolver{Server: dc.DNS, Timeout: secondsDur(dc.Timeouts.ConnectS)}
		}
		d := NewDeliverer(runner, tr, res, dc.Hostname)
		onAccept = d.Wake
		go func() { delErr <- d.Run(ctx) }()
	}
	srv := &http.Server{Handler: NewAPI(runner, onAccept), ReadHeaderTimeout: 10 * time.Second}
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
	case result = <-delErr:
		delivering = false
	}
	shut, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	srv.Shutdown(shut)
	cancel()
	if delivering {
		<-delErr // let connection goroutines finish before the store closes
	}
	if errors.Is(result, context.Canceled) || errors.Is(result, http.ErrServerClosed) {
		result = nil
	}
	return result
}
