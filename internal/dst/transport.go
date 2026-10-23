// Package dst is the deterministic simulation of mtadst (contract 2.2,
// D05): the engine core and the simulator's session machine in one
// process, on a virtual clock, driven by the engine's Stepper.
package dst

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/textproto"
	"sort"
	"time"

	"github.com/CesarManzoCode/mta-proof/internal/engine"
	"github.com/CesarManzoCode/mta-proof/internal/sim"
)

// Transport is the engine Transport of the simulated executor. A dial
// opens a session of the receiver on the host that owns the destination
// address in the lab zone, as mtasim listens on it; commands and message
// data go through the session machine and its replies come back as wire
// bytes parsed by the engine's reply reader. Network transfer takes no
// virtual time; the profile latency of a reply is spent with the
// connection's Sleeper, so it advances the virtual clock of that
// connection only.
type Transport struct {
	r       *sim.Receiver
	hosts   map[netip.Addr]string
	cmd     time.Duration
	dataEnd time.Duration
	open    map[int64]*conn
}

// NewTransport serves every host of r on its A addresses in zone. Replies
// slower than the engine's command or end-of-data timeout are cut at the
// timeout, as the real client gives up.
func NewTransport(r *sim.Receiver, zone *sim.Zone, cmdTimeout, dataEndTimeout time.Duration) (*Transport, error) {
	t := &Transport{r: r, hosts: map[netip.Addr]string{}, cmd: cmdTimeout, dataEnd: dataEndTimeout, open: map[int64]*conn{}}
	for _, h := range r.Hosts() {
		addrs := zone.Addrs(h)
		if len(addrs) == 0 {
			return nil, fmt.Errorf("host %s (profile %s) has no A record in the zone", h, r.ProfileOf(h))
		}
		for _, a := range addrs {
			if other, ok := t.hosts[a]; ok {
				return nil, fmt.Errorf("address %s belongs to hosts %s and %s", a, other, h)
			}
			t.hosts[a] = h
		}
	}
	return t, nil
}

var (
	errRefused = errors.New("connection refused")
	errStopped = errors.New("simulation stopped")
	errTimeout = errors.New("i/o timeout")
)

// Dial opens a session from src to the host of dst. An address no
// profile host owns refuses the connection at once.
func (t *Transport) Dial(ctx context.Context, src netip.Addr, dst netip.AddrPort) (engine.Conn, error) {
	if ctx.Err() != nil {
		return nil, errStopped
	}
	host, ok := t.hosts[dst.Addr()]
	if !ok {
		return nil, errRefused
	}
	sess, greeting, err := t.r.Open(host, src, dst.Addr())
	if err != nil {
		return nil, err
	}
	c := &conn{t: t, sess: sess, sleep: engine.SleeperFrom(ctx), greeting: greeting}
	t.open[sess.ID()] = c
	return c, nil
}

// Shutdown ends the sessions still open, in connection order, as mtasim
// does when it stops: each gets a drop with rule shutdown.
func (t *Transport) Shutdown() {
	ids := make([]int64, 0, len(t.open))
	for id := range t.open {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		c := t.open[id]
		c.sess.Cut(sim.ShutdownRule)
		c.gone()
	}
}

type conn struct {
	t        *Transport
	sess     *sim.Session
	sleep    engine.Sleeper
	greeting sim.Result
	closed   bool
	drop     bool // the 354 asked to cut the data short (drop_mid_data)
}

func (c *conn) gone() {
	c.closed = true
	delete(c.t.open, c.sess.ID())
}

// reply delivers a session result to the client: it waits for the profile
// latency (at most timeout), then returns the reply as the engine reads it
// from the wire. A result without a reply is a close without one.
func (c *conn) reply(res sim.Result, timeout time.Duration) (engine.Reply, error) {
	if res.Reply == nil {
		c.sess.Gone()
		c.gone()
		return engine.Reply{}, io.EOF
	}
	if timeout > 0 && res.Delay > timeout {
		// The client gives up before the reply arrives and closes.
		if !c.wait(timeout) {
			return engine.Reply{}, errStopped
		}
		c.sess.Gone()
		c.gone()
		return engine.Reply{}, errTimeout
	}
	if !c.wait(res.Delay) {
		return engine.Reply{}, errStopped
	}
	if res.Close {
		c.gone()
	}
	return engine.ReadReply(textproto.NewReader(bufio.NewReader(bytes.NewReader(res.Wire()))))
}

func (c *conn) wait(d time.Duration) bool {
	if d <= 0 {
		return true
	}
	if c.sleep == nil {
		return false
	}
	return c.sleep.Sleep(d)
}

func (c *conn) Greeting() (engine.Reply, error) { return c.reply(c.greeting, c.t.cmd) }

func (c *conn) Cmd(line string) (engine.Reply, error) {
	if c.closed {
		return engine.Reply{}, io.EOF
	}
	res := c.sess.Line(line)
	if res.Reply != nil && res.Reply.Code == 354 {
		c.drop = res.DropMidData
	}
	return c.reply(res, c.t.cmd)
}

// Data encodes msg as the real client does (dot-stuffing and the final
// ".") and decodes it as the socket server does, so the session stores the
// same bytes in both modes.
func (c *conn) Data(msg []byte) (engine.Reply, error) {
	if c.closed {
		return engine.Reply{}, io.EOF
	}
	if c.drop {
		c.drop = false
		c.sess.DropData()
		c.gone()
		return engine.Reply{}, io.EOF
	}
	var wire bytes.Buffer
	bw := bufio.NewWriter(&wire)
	w := textproto.NewWriter(bw).DotWriter()
	if _, err := w.Write(msg); err != nil {
		return engine.Reply{}, err
	}
	if err := w.Close(); err != nil {
		return engine.Reply{}, err
	}
	data, oversize, err := sim.DecodeData(wire.Bytes())
	if err != nil {
		return engine.Reply{}, err
	}
	var res sim.Result
	if oversize {
		res = c.sess.DataOversize()
	} else {
		res = c.sess.Data(data)
	}
	return c.reply(res, c.t.dataEnd)
}

// Close ends the connection from the client side; a session the receiver
// had not ended logs a close.
func (c *conn) Close() error {
	if !c.closed {
		c.sess.Gone()
		c.gone()
	}
	return nil
}
