package dst

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/miekg/dns"

	"github.com/CesarManzoCode/mta-proof/internal/clock"
	"github.com/CesarManzoCode/mta-proof/internal/engine"
	"github.com/CesarManzoCode/mta-proof/internal/sim"
	"github.com/CesarManzoCode/mta-proof/internal/store"
)

// Options configures one simulated run.
type Options struct {
	Root     string // repository root: scenario paths are relative to it
	Research string // RESEARCH file that checks profile sources; empty: syntax only
	Scenario *Scenario
	Seed     uint64
	Out      string // run directory; must not exist or be empty
	MaxSteps int    // event budget; 0: 100 million
}

// Summary describes a finished run.
type Summary struct {
	Scenario   string         `json:"scenario"`
	Variant    string         `json:"variant,omitempty"`
	Seed       uint64         `json:"seed"`
	Messages   int            `json:"messages"`
	States     map[string]int `json:"states"`
	Events     int            `json:"events"`
	Steps      int64          `json:"steps"`
	VirtualMs  int64          `json:"virtual_ms"`
	Complete   bool           `json:"complete"` // nothing was left to happen
	WallMs     int64          `json:"wall_ms"`
	TraceSHA   string         `json:"trace_sha256"`
	ReceiverLn int            `json:"receiver_lines"`
}

// Run executes a scenario in one process on a virtual clock that starts at
// 0 (contract 3.6: at_ms are virtual milliseconds since the start) and
// writes the run directory of 3.6: receiver.jsonl, messages/, engine.db,
// events.jsonl and trace.sha256.
func Run(o Options) (*Summary, error) {
	wall := time.Now()
	sc := o.Scenario
	if o.MaxSteps == 0 {
		o.MaxSteps = 100_000_000
	}
	if err := emptyDir(o.Out); err != nil {
		return nil, err
	}
	cfg, err := sc.EngineConfig(o.Root)
	if err != nil {
		return nil, err
	}
	cfg.Seed = o.Seed
	var idx sim.SourceIndex
	if o.Research != "" {
		if idx, err = sim.LoadResearch(filepath.Join(o.Root, o.Research)); err != nil {
			return nil, err
		}
	}
	all, err := sim.LoadProfiles(filepath.Join(o.Root, sc.ProfilesDir), idx)
	if err != nil {
		return nil, err
	}
	profiles := map[string]*sim.Profile{}
	for _, name := range sc.Profiles {
		p, ok := all[name]
		if !ok {
			return nil, fmt.Errorf("scenario %s: unknown profile %q", sc.Name, name)
		}
		profiles[name] = p
	}
	zone, err := sim.LoadZone(filepath.Join(o.Root, sc.Zone))
	if err != nil {
		return nil, err
	}

	clk := clock.NewVirtual(time.UnixMilli(0))
	recv, err := sim.NewReceiver(sim.Config{Dir: o.Out, Profiles: profiles, Clock: clk, NoSync: true})
	if err != nil {
		return nil, err
	}
	recvOpen := true
	defer func() {
		if recvOpen {
			recv.Close()
		}
	}()
	tr, err := NewTransport(recv, zone, secondsDur(cfg.Timeouts.CommandS), secondsDur(cfg.Timeouts.DataEndS))
	if err != nil {
		return nil, err
	}
	st, err := store.OpenUnsynced(filepath.Join(o.Out, "engine.db"))
	if err != nil {
		return nil, err
	}
	defer st.Close()
	core, err := engine.OpenCore(st, clk, cfg.Seed)
	if err != nil {
		return nil, err
	}
	core.SetDelivery(cfg, clock.Rand(cfg.Seed, "engine.mx"))
	step := &engine.Stepper{Core: core, Store: st, Clock: clk, Transport: tr, Helo: cfg.Hostname,
		Resolver: &engine.DNSResolver{Exchange: func(_ context.Context, m *dns.Msg) (*dns.Msg, error) {
			return zone.Answer(m), nil
		}}}
	arrivals := sc.Arrivals(cfg.Seed)
	for _, a := range arrivals {
		sub := a.Sub
		step.At(time.UnixMilli(a.AtMs), func() error {
			if err := sub.Validate(); err != nil {
				return fmt.Errorf("scenario %s: message to %s: %w", sc.Name, sub.To, err)
			}
			core.Ingest(&sub, "")
			return nil
		})
	}
	if err := step.Run(time.UnixMilli(sc.durationMs()), o.MaxSteps); err != nil {
		return nil, err
	}
	complete := step.Idle()
	tr.Shutdown()
	step.Stop()
	recvOpen = false
	if err := recv.Close(); err != nil {
		return nil, err
	}

	evs, err := st.Events()
	if err != nil {
		return nil, err
	}
	if err := writeEvents(filepath.Join(o.Out, "events.jsonl"), evs); err != nil {
		return nil, err
	}
	sum, err := TraceHash(o.Out)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(o.Out, "trace.sha256"), []byte(sum+"\n"), 0o644); err != nil {
		return nil, err
	}
	snap, err := st.Load()
	if err != nil {
		return nil, err
	}
	states := map[string]int{}
	for _, m := range snap.Messages {
		states[m.State]++
	}
	lines, err := countLines(filepath.Join(o.Out, "receiver.jsonl"))
	if err != nil {
		return nil, err
	}
	return &Summary{Scenario: sc.Name, Variant: sc.Variant, Seed: o.Seed, Messages: len(snap.Messages), States: states,
		Events: len(evs), Steps: step.Executed(), VirtualMs: clock.Ms(clk.Now()), Complete: complete,
		WallMs: time.Since(wall).Milliseconds(), TraceSHA: sum, ReceiverLn: lines}, nil
}

func secondsDur(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// emptyDir creates dir, or accepts it if it exists and is empty, so a run
// directory never mixes two runs.
func emptyDir(dir string) error {
	if dir == "" {
		return errors.New("out directory is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(ents) > 0 {
		return fmt.Errorf("out directory %s is not empty", dir)
	}
	return nil
}

func countLines(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n, nil
}

// TraceHash is the hex sha256 of the bytes of receiver.jsonl followed by
// those of events.jsonl in dir (contract 3.6).
func TraceHash(dir string) (string, error) {
	h := sha256.New()
	for _, name := range []string{"receiver.jsonl", "events.jsonl"} {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
