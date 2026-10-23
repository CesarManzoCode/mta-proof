package engine

import (
	"context"
	"errors"
	"time"

	"github.com/CesarManzoCode/mta-proof/internal/clock"
	"github.com/CesarManzoCode/mta-proof/internal/store"
)

// Committer makes a group of core writes durable.
type Committer interface {
	Apply([]store.Write) error
}

// Stepper is a single-goroutine executor with a virtual clock: it runs
// the core's actions one at a time in FIFO order against a Transport and a
// Resolver, commits the core's writes after every step, before acting on
// what the step returned, and moves the clock to the next due instant when
// nothing is left to do. With deterministic transport and resolver, the
// same seed gives the same events.
type Stepper struct {
	Core      *Core
	Store     Committer
	Clock     *clock.Virtual
	Transport Transport
	Resolver  Resolver
	Helo      string

	fifo  []Action
	conns map[int64]Conn
}

// ErrStepLimit is returned by Run when it runs out of steps.
var ErrStepLimit = errors.New("engine: stepper step limit")

func (s *Stepper) commit() error { return s.Store.Apply(s.Core.TakeWrites()) }

// Run executes until nothing is due before until, or maxSteps actions ran.
func (s *Stepper) Run(until time.Time, maxSteps int) error {
	if s.conns == nil {
		s.conns = map[int64]Conn{}
	}
	ctx := context.Background()
	for steps := 0; ; steps++ {
		if steps >= maxSteps {
			return ErrStepLimit
		}
		acts, wake := s.Core.Dispatch()
		if err := s.commit(); err != nil {
			return err
		}
		s.fifo = append(s.fifo, acts...)
		if len(s.fifo) == 0 {
			if wake == 0 || time.UnixMilli(wake).After(until) {
				return nil
			}
			s.Clock.Set(time.UnixMilli(wake))
			continue
		}
		a := s.fifo[0]
		s.fifo = s.fifo[1:]
		var next []Action
		switch a.Kind {
		case ActResolve:
			s.Core.Resolved(a.Domain, s.Resolver.Resolve(ctx, a.Domain))
		case ActOpen:
			conn, res := openConn(ctx, s.Transport, a.Open, s.Helo)
			if conn != nil {
				s.conns[a.Conn] = conn
			}
			next = s.Core.Opened(a.Conn, res)
		case ActSend:
			tr := transact(s.conns[a.Conn], a.Send.From, a.Send.To, a.Send.Raw)
			next = s.Core.Sent(a.Conn, a.Send.MsgID, tr)
		case ActClose:
			if c := s.conns[a.Conn]; c != nil {
				quit(c)
				delete(s.conns, a.Conn)
			}
			s.Core.Closed(a.Conn)
		}
		if err := s.commit(); err != nil {
			return err
		}
		s.fifo = append(s.fifo, next...)
	}
}
