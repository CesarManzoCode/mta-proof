package engine

import (
	"container/heap"
	"context"
	"errors"
	"iter"
	"sort"
	"time"

	"github.com/CesarManzoCode/mta-proof/internal/clock"
	"github.com/CesarManzoCode/mta-proof/internal/store"
)

// Committer makes a group of core writes durable.
type Committer interface {
	Apply([]store.Write) error
}

// Sleeper lets a transport spend virtual time inside a connection of the
// Stepper, such as the latency a receiver profile adds before a reply.
// Sleep returns false once the stepper stopped; the transport must then
// fail the call without sleeping again.
type Sleeper interface {
	Sleep(d time.Duration) bool
}

type sleeperKey struct{}

// SleeperFrom returns the Sleeper of the connection being dialled with ctx,
// or nil outside the Stepper. A transport keeps it for the life of the
// connection.
func SleeperFrom(ctx context.Context) Sleeper {
	s, _ := ctx.Value(sleeperKey{}).(Sleeper)
	return s
}

// Stepper is the simulated executor (contract 2.2, D05): a queue of events
// ordered by (virtual time, sequence) on one logical thread. An event is a
// dispatch of the core, one action of the core, an external input (At) or
// the resumption of a connection that was waiting. The core's writes are
// committed after every event, before acting on what the event returned.
//
// The conversation of each connection runs as a coroutine (iter.Pull): it
// runs only while the stepper waits for it, and it hands control back when
// its transport sleeps, which schedules its resumption at now+d. So
// connections overlap in virtual time while exactly one piece of code runs
// at any moment, and the order of everything is the order of the queue.
// With a deterministic transport and resolver, the same inputs give the
// same events.
type Stepper struct {
	Core      *Core
	Store     Committer
	Clock     *clock.Virtual
	Transport Transport
	Resolver  Resolver
	Helo      string

	q        eventHeap
	seq      uint64
	pending  map[int64]bool // instants with a dispatch already queued
	conns    map[int64]*proc
	ctx      context.Context
	cancel   context.CancelFunc
	err      error
	stopped  bool
	executed int64
}

// ErrStepLimit is returned by Run when it runs out of steps.
var ErrStepLimit = errors.New("engine: stepper step limit")

type event struct {
	at  int64
	seq uint64
	fn  func() error
}

type eventHeap []event

func (h eventHeap) Len() int { return len(h) }
func (h eventHeap) Less(i, j int) bool {
	if h[i].at != h[j].at {
		return h[i].at < h[j].at
	}
	return h[i].seq < h[j].seq
}
func (h eventHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *eventHeap) Push(x any)   { *h = append(*h, x.(event)) }
func (h *eventHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	*h = old[:len(old)-1]
	return e
}

// proc is the coroutine of one connection's current operation (open, one
// transaction or close) and the connection it owns.
type proc struct {
	s       *Stepper
	id      int64
	conn    Conn
	next    func() (int64, bool)
	stop    func()
	yield   func(int64) bool
	finish  func() []Action
	stopped bool
}

// Sleep suspends the operation until now+d of virtual time, rounded up to
// the millisecond of at_ms.
func (p *proc) Sleep(d time.Duration) bool {
	if p.stopped || p.yield == nil {
		return false
	}
	ms := int64((d + time.Millisecond - 1) / time.Millisecond)
	if ms <= 0 {
		return true
	}
	if !p.yield(p.s.now() + ms) {
		p.stopped = true
		return false
	}
	return true
}

func (s *Stepper) init() {
	if s.pending != nil {
		return
	}
	s.pending = map[int64]bool{}
	s.conns = map[int64]*proc{}
	s.ctx, s.cancel = context.WithCancel(context.Background())
}

func (s *Stepper) now() int64 { return clock.Ms(s.Clock.Now()) }

func (s *Stepper) commit() error { return s.Store.Apply(s.Core.TakeWrites()) }

func (s *Stepper) push(at int64, fn func() error) {
	s.seq++
	heap.Push(&s.q, event{at: at, seq: s.seq, fn: fn})
}

// At schedules fn, an external input such as a message arrival, at virtual
// instant t (or now, if t has passed). Its writes are committed when it
// returns, and a dispatch follows.
func (s *Stepper) At(t time.Time, fn func() error) {
	s.init()
	s.push(max(clock.Ms(t), s.now()), func() error {
		if err := fn(); err != nil {
			return err
		}
		return s.done(nil)
	})
}

// dispatchAt queues a dispatch of the core at instant at, unless one is
// already queued there.
func (s *Stepper) dispatchAt(at int64) {
	if s.pending[at] {
		return
	}
	s.pending[at] = true
	s.push(at, func() error {
		delete(s.pending, at)
		acts, wake := s.Core.Dispatch()
		if err := s.commit(); err != nil {
			return err
		}
		s.schedule(acts)
		if wake > 0 {
			s.dispatchAt(wake)
		}
		return nil
	})
}

// schedule queues the actions now, in the order the core gave them.
func (s *Stepper) schedule(acts []Action) {
	now := s.now()
	for _, a := range acts {
		s.push(now, func() error { return s.act(a) })
	}
}

// done commits what an event changed, queues the actions it produced and
// a dispatch, since the change may let other queues progress.
func (s *Stepper) done(acts []Action) error {
	if err := s.commit(); err != nil {
		return err
	}
	s.schedule(acts)
	s.dispatchAt(s.now())
	return nil
}

// Run executes events until none is due at or before until, or maxSteps
// events ran. Events after until stay queued for the next Run. The clock
// ends at the last event executed.
func (s *Stepper) Run(until time.Time, maxSteps int) error {
	s.init()
	if s.err != nil {
		return s.err
	}
	if s.stopped {
		return ErrStopped
	}
	end := clock.Ms(until)
	s.dispatchAt(s.now())
	for steps := 0; ; steps++ {
		if len(s.q) == 0 || s.q[0].at > end {
			return nil
		}
		if steps >= maxSteps {
			return ErrStepLimit
		}
		e := heap.Pop(&s.q).(event)
		s.Clock.Set(time.UnixMilli(e.at))
		s.executed++
		if err := e.fn(); err != nil {
			s.err = err
			return err
		}
	}
}

// Idle reports whether no event is queued: everything the core scheduled
// has happened.
func (s *Stepper) Idle() bool { return len(s.q) == 0 }

// Executed returns the number of events run so far.
func (s *Stepper) Executed() int64 { return s.executed }

// Stop ends, in connection order, the operations still waiting inside a
// conversation, without reporting their outcome to the core: their
// attempts stay inflight, as after a crash, and recovery would requeue
// them. The stepper cannot run again.
func (s *Stepper) Stop() {
	s.init()
	s.stopped = true
	s.cancel()
	ids := make([]int64, 0, len(s.conns))
	for id := range s.conns {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if p := s.conns[id]; p.stop != nil {
			p.stop()
		}
	}
}

func (s *Stepper) act(a Action) error {
	switch a.Kind {
	case ActResolve:
		s.Core.Resolved(a.Domain, s.Resolver.Resolve(s.ctx, a.Domain))
		return s.done(nil)
	case ActOpen:
		p := &proc{s: s, id: a.Conn}
		s.conns[a.Conn] = p
		var res OpenResult
		return s.start(p, func() {
			p.conn, res = openConn(context.WithValue(s.ctx, sleeperKey{}, p), s.Transport, a.Open, s.Helo)
		}, func() []Action {
			if p.conn == nil {
				delete(s.conns, a.Conn)
			}
			return s.Core.Opened(a.Conn, res)
		})
	case ActSend:
		p := s.conns[a.Conn]
		if p == nil || p.conn == nil {
			return s.done(s.Core.Sent(a.Conn, a.Send.MsgID,
				TxResult{Stage: "mail", Err: "no open connection", Closed: true}))
		}
		var tr TxResult
		return s.start(p, func() {
			tr = transact(p.conn, a.Send.From, a.Send.To, a.Send.Raw)
		}, func() []Action {
			return s.Core.Sent(a.Conn, a.Send.MsgID, tr)
		})
	case ActClose:
		p := s.conns[a.Conn]
		finish := func() []Action {
			delete(s.conns, a.Conn)
			s.Core.Closed(a.Conn)
			return nil
		}
		if p == nil || p.conn == nil {
			return s.done(finish())
		}
		return s.start(p, func() { quit(p.conn) }, finish)
	}
	return nil
}

// start runs the network part of an operation as a coroutine of p; finish
// reports its outcome to the core once it completes.
func (s *Stepper) start(p *proc, run func(), finish func() []Action) error {
	p.finish = finish
	p.next, p.stop = iter.Pull(func(yield func(int64) bool) {
		p.yield = yield
		run()
	})
	return s.resume(p)
}

// resume lets p's operation run until it sleeps, which queues its next
// resumption, or completes.
func (s *Stepper) resume(p *proc) error {
	at, sleeping := p.next()
	if sleeping {
		s.push(at, func() error { return s.resume(p) })
		return nil
	}
	p.next, p.stop, p.yield = nil, nil, nil
	return s.done(p.finish())
}
