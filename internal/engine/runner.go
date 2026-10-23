package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/CesarManzoCode/mta-proof/internal/store"
)

// ErrStopped is returned for operations submitted after the runner stopped
// or whose group failed to commit.
var ErrStopped = errors.New("engine: runner stopped")

// Runner is the real-mode executor of the core: one goroutine owns the
// core, runs operations in arrival order and commits the writes of every
// operation that queued up while the previous commit was in progress in a
// single transaction (group commit, contract 2.4). No caller sees a result
// before the transaction holding its writes is committed.
type Runner struct {
	core     *Core
	st       *store.Store
	ops      chan *op
	maxBatch int
	done     chan struct{}

	mu     sync.Mutex
	err    error
	groups int
	opsN   int
}

type op struct {
	fn    func(*Core) any
	reply chan result
}

type result struct {
	v   any
	err error
}

// NewRunner returns a runner for a core whose start-up writes (recovery)
// are already committed. maxBatch bounds the operations in one commit.
func NewRunner(core *Core, st *store.Store, maxBatch int) *Runner {
	if maxBatch <= 0 {
		maxBatch = 256
	}
	return &Runner{core: core, st: st, ops: make(chan *op, maxBatch), maxBatch: maxBatch, done: make(chan struct{})}
}

// Run executes operations until ctx is done or a commit fails. A failed
// commit leaves the in-memory core ahead of the database, so the runner
// stops and the process must restart from the store.
func (r *Runner) Run(ctx context.Context) error {
	defer close(r.done)
	for {
		var first *op
		select {
		case <-ctx.Done():
			return ctx.Err()
		case first = <-r.ops:
		}
		batch := []*op{first}
	drain:
		for len(batch) < r.maxBatch {
			select {
			case o := <-r.ops:
				batch = append(batch, o)
			default:
				break drain
			}
		}
		results := make([]any, len(batch))
		for i, o := range batch {
			results[i] = o.fn(r.core)
		}
		err := r.st.Apply(r.core.TakeWrites())
		r.mu.Lock()
		r.groups++
		r.opsN += len(batch)
		if err != nil {
			r.err = fmt.Errorf("engine: group commit: %w", err)
		}
		r.mu.Unlock()
		for i, o := range batch {
			if err != nil {
				o.reply <- result{err: ErrStopped}
			} else {
				o.reply <- result{v: results[i]}
			}
		}
		if err != nil {
			return r.err
		}
	}
}

// Do runs fn on the core and returns its value once fn's writes are
// committed.
func (r *Runner) Do(ctx context.Context, fn func(*Core) any) (any, error) {
	o := &op{fn: fn, reply: make(chan result, 1)}
	select {
	case r.ops <- o:
	case <-r.done:
		return nil, ErrStopped
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case res := <-o.reply:
		return res.v, res.err
	case <-r.done:
		// Run may have replied just before closing done.
		select {
		case res := <-o.reply:
			return res.v, res.err
		default:
			return nil, ErrStopped
		}
	}
}

// Ingest admits a validated submission through the core.
func (r *Runner) Ingest(ctx context.Context, s *Submission, key string) (IngestResult, error) {
	v, err := r.Do(ctx, func(c *Core) any { return c.Ingest(s, key) })
	if err != nil {
		return IngestResult{}, err
	}
	return v.(IngestResult), nil
}

// Stats returns the number of commits and of operations committed so far.
func (r *Runner) Stats() (groups, ops int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.groups, r.opsN
}
