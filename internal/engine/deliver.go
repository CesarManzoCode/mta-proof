package engine

import (
	"context"
	"crypto/x509"
	"sync"
	"time"
)

// Deliverer is the real-mode delivery executor. Every interaction with the
// core goes through the Runner, whose Do returns only after the writes of
// the operation are committed: an attempt is durable before its SMTP
// transaction starts, and its result is durable before the connection or
// the scheduler does anything else with the message (contract 2.4).
type Deliverer struct {
	r    *Runner
	tr   Transport
	res  Resolver
	helo string
	wake chan struct{}
	// Roots are the CAs a STARTTLS certificate must chain to (tls.ca_file).
	Roots *x509.CertPool

	mu    sync.Mutex
	inbox map[int64]chan Action
	wg    sync.WaitGroup
}

// NewDeliverer returns a deliverer over a runner.
func NewDeliverer(r *Runner, tr Transport, res Resolver, helo string) *Deliverer {
	return &Deliverer{r: r, tr: tr, res: res, helo: helo, wake: make(chan struct{}, 1), inbox: map[int64]chan Action{}}
}

// Wake asks for a dispatch soon; it never blocks.
func (d *Deliverer) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

type dispatched struct {
	acts []Action
	wake int64
}

// Run dispatches until ctx is done or the runner stops, then waits for the
// connection goroutines. Attempts still in flight stay inflight in the
// store and are recovered at the next start.
func (d *Deliverer) Run(ctx context.Context) error {
	defer d.wg.Wait()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-d.wake:
		case <-timer.C:
		}
		v, err := d.r.Do(ctx, func(c *Core) any {
			acts, wake := c.Dispatch()
			return dispatched{acts, wake}
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		res := v.(dispatched)
		d.handle(ctx, res.acts)
		wait := time.Hour
		if res.wake > 0 {
			wait = max(time.Until(time.UnixMilli(res.wake)), 0)
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
	}
}

// handle starts resolutions and connections and hands sends and closes to
// the goroutine of their connection.
func (d *Deliverer) handle(ctx context.Context, acts []Action) {
	for _, a := range acts {
		switch a.Kind {
		case ActResolve:
			d.wg.Add(1)
			go func() {
				defer d.wg.Done()
				res := d.res.Resolve(ctx, a.Domain)
				if _, err := d.r.Do(ctx, func(c *Core) any { c.Resolved(a.Domain, res); return nil }); err == nil {
					d.Wake()
				}
			}()
		case ActOpen:
			ch := make(chan Action, 4)
			d.mu.Lock()
			d.inbox[a.Conn] = ch
			d.mu.Unlock()
			d.wg.Add(1)
			go d.conn(ctx, a, ch)
		case ActSend, ActClose:
			d.mu.Lock()
			ch := d.inbox[a.Conn]
			d.mu.Unlock()
			if ch != nil {
				ch <- a
			}
		}
	}
}

func (d *Deliverer) do(ctx context.Context, fn func(*Core) []Action) ([]Action, bool) {
	v, err := d.r.Do(ctx, func(c *Core) any { return fn(c) })
	if err != nil {
		return nil, false
	}
	acts, _ := v.([]Action)
	return acts, true
}

// conn owns one connection: it opens it, then runs the sends and the close
// the core gives it, one at a time.
func (d *Deliverer) conn(ctx context.Context, open Action, ch chan Action) {
	defer d.wg.Done()
	defer func() {
		d.mu.Lock()
		delete(d.inbox, open.Conn)
		d.mu.Unlock()
	}()
	conn, res := openConn(ctx, d.tr, open.Open, d.helo, d.Roots)
	if conn != nil {
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		defer stop()
	}
	acts, ok := d.do(ctx, func(c *Core) []Action { return c.Opened(open.Conn, res) })
	if !ok || conn == nil {
		if conn != nil {
			conn.Close()
		}
		d.Wake()
		return
	}
	d.handle(ctx, acts)
	for {
		var a Action
		select {
		case <-ctx.Done():
			conn.Close()
			return
		case a = <-ch:
		}
		switch a.Kind {
		case ActSend:
			tr := transact(conn, a.Send.From, a.Send.To, a.Send.Raw)
			acts, ok := d.do(ctx, func(c *Core) []Action { return c.Sent(open.Conn, a.Send.MsgID, tr) })
			if !ok {
				conn.Close()
				return
			}
			d.handle(ctx, acts)
			d.Wake()
		case ActClose:
			quit(conn)
			d.do(ctx, func(c *Core) []Action { c.Closed(open.Conn); return nil })
			d.Wake()
			return
		}
	}
}
