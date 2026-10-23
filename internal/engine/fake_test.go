package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CesarManzoCode/mta-proof/internal/clock"
	"github.com/CesarManzoCode/mta-proof/internal/store"
)

// fakeHost is one scripted SMTP server of the fake network.
type fakeHost struct {
	name     string
	down     bool   // dials fail
	greeting *Reply // default 220
	// silentAfter closes the connection without a reply at the MAIL that
	// follows this many accepted MAILs on the connection (R-YAHOO).
	silentAfter int
	mail        func(h *fakeHost, now int64) *Reply // nil reply: 250
	rcpt        func(to string) *Reply
	dataEnd     func(raw []byte) *Reply

	open, maxOpen int
	perConn       []int // accepted MAILs of each closed or open connection
	mails         []int64
	got           []fakeMsg
}

type fakeMsg struct {
	from, to, id string
	at           int64
	raw          []byte
}

// fakeNet is a Transport over scripted hosts keyed by address; ports are
// ignored. It checks the durability rule of contract 2.4 on every
// transaction: when the message reaches DATA, the last committed event of
// the message must belong to the attempt in flight.
type fakeNet struct {
	mu      sync.Mutex // the real executor calls from many goroutines
	t       *testing.T
	clk     *clock.Virtual
	hosts   map[netip.Addr]*fakeHost
	dials   []string // "host addr" in dial order
	open    int
	maxOpen int
	st      *store.Store
}

func (n *fakeNet) add(h *fakeHost, addr string) {
	n.hosts[netip.MustParseAddr(addr)] = h
}

func (n *fakeNet) Dial(_ context.Context, src netip.Addr, dst netip.AddrPort) (Conn, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	h := n.hosts[dst.Addr()]
	name := "?"
	if h != nil {
		name = h.name
	}
	n.dials = append(n.dials, name+" "+dst.Addr().String())
	n.checkOpener()
	if h == nil || h.down {
		return nil, errors.New("connection refused")
	}
	n.open++
	n.maxOpen = max(n.maxOpen, n.open)
	h.open++
	h.maxOpen = max(h.maxOpen, h.open)
	h.perConn = append(h.perConn, 0)
	return &fakeConn{n: n, h: h, idx: len(h.perConn) - 1}, nil
}

type fakeConn struct {
	n        *fakeNet
	h        *fakeHost
	idx      int
	closed   bool
	from, to string
}

func (c *fakeConn) now() int64 { return clock.Ms(c.n.clk.Now()) }

func (c *fakeConn) Greeting() (Reply, error) {
	c.n.mu.Lock()
	defer c.n.mu.Unlock()
	if c.h.greeting != nil {
		g := *c.h.greeting
		if g.Code == 421 {
			c.close()
		}
		return g, nil
	}
	return Reply{Code: 220, Text: c.h.name + " ESMTP"}, nil
}

func ok250() (Reply, error) { return Reply{Code: 250, Enhanced: "2.0.0", Text: "OK"}, nil }

func (c *fakeConn) reply(r *Reply) (Reply, error) {
	if r == nil {
		return ok250()
	}
	if r.Code == 421 {
		c.close()
	}
	return *r, nil
}

func (c *fakeConn) Cmd(line string) (Reply, error) {
	c.n.mu.Lock()
	defer c.n.mu.Unlock()
	if c.closed {
		return Reply{}, io.EOF
	}
	verb, arg, _ := strings.Cut(line, " ")
	switch verb {
	case "EHLO", "HELO", "RSET":
		return ok250()
	case "QUIT":
		c.close()
		return Reply{Code: 221, Text: "bye"}, nil
	case "MAIL":
		if c.h.silentAfter > 0 && c.h.perConn[c.idx] >= c.h.silentAfter {
			c.close()
			return Reply{}, io.EOF
		}
		if c.h.mail != nil {
			if r := c.h.mail(c.h, c.now()); r != nil {
				return c.reply(r)
			}
		}
		c.h.perConn[c.idx]++
		c.h.mails = append(c.h.mails, c.now())
		c.from = strings.TrimSuffix(strings.TrimPrefix(arg, "FROM:<"), ">")
		return ok250()
	case "RCPT":
		c.to = strings.TrimSuffix(strings.TrimPrefix(arg, "TO:<"), ">")
		if c.h.rcpt != nil {
			return c.reply(c.h.rcpt(c.to))
		}
		return ok250()
	case "DATA":
		return Reply{Code: 354, Text: "go ahead"}, nil
	}
	return Reply{Code: 500, Text: "unknown"}, nil
}

func (c *fakeConn) Data(raw []byte) (Reply, error) {
	c.n.mu.Lock()
	defer c.n.mu.Unlock()
	if c.closed {
		return Reply{}, io.EOF
	}
	id := headerValue(raw, "X-Mtaproof-Id")
	c.n.checkInFlight(id)
	c.h.got = append(c.h.got, fakeMsg{from: c.from, to: c.to, id: id, at: c.now(), raw: raw})
	if c.h.dataEnd != nil {
		return c.reply(c.h.dataEnd(raw))
	}
	return ok250()
}

func (c *fakeConn) Close() error {
	c.n.mu.Lock()
	defer c.n.mu.Unlock()
	c.close()
	return nil
}

func (c *fakeConn) close() {
	if !c.closed {
		c.closed = true
		c.n.open--
		c.h.open--
	}
}

func headerValue(raw []byte, name string) string {
	for _, line := range strings.Split(string(raw), "\r\n") {
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, name+": "); ok {
			return v
		}
	}
	return ""
}

// checkInFlight verifies that the attempt of id is committed and has no
// committed result yet.
func (n *fakeNet) checkInFlight(id string) {
	n.t.Helper()
	if n.st == nil {
		return
	}
	evs, err := n.st.Events()
	if err != nil {
		n.t.Error(err)
		return
	}
	last := ""
	for _, e := range evs {
		if e.MsgID == id {
			last = e.Kind
		}
	}
	if last != KindAttemptStarted && last != KindRouted {
		n.t.Errorf("DATA of %s with last committed event %q: the attempt is not durable", id, last)
	}
}

// checkOpener verifies, when a connection is dialled, that some message
// has a committed attempt_started and nothing after it: the attempt that
// opens the connection is durable before the network is touched.
func (n *fakeNet) checkOpener() {
	n.t.Helper()
	if n.st == nil {
		return
	}
	evs, err := n.st.Events()
	if err != nil {
		n.t.Error(err)
		return
	}
	last := map[string]string{}
	for _, e := range evs {
		last[e.MsgID] = e.Kind
	}
	for _, k := range last {
		if k == KindAttemptStarted {
			return
		}
	}
	n.t.Error("dial without a committed attempt_started awaiting its result")
}

// fakeResolver answers from a map and counts lookups.
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string]Resolution
	lookups map[string]int
}

func (r *fakeResolver) Resolve(_ context.Context, domain string) Resolution {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lookups[domain]++
	if a, ok := r.answers[domain]; ok {
		return a
	}
	return Resolution{Status: ResolvedNXDomain, Err: "no such domain"}
}

func mx(pref uint16, host string, addrs ...string) MXHost {
	h := MXHost{Pref: pref, Host: host}
	for _, a := range addrs {
		h.Addrs = append(h.Addrs, netip.MustParseAddr(a))
	}
	return h
}

// testConfig is a small valid configuration for the fake network.
func testConfig() *Config {
	cfg := &Config{DNS: "127.0.0.1:53", Hostname: "engine.mtaproof.test", Postmaster: "postmaster@engine.mtaproof.test",
		SrcIPs: []string{"127.0.2.1"}, Retry: RetryPolicy{InitialS: 60, Factor: 2, MaxS: 600, LifetimeS: 3600},
		Providers: []Provider{{Name: "yahoo-model", MXSuffixes: []string{"yahoo-model.test"}}}}
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	return cfg
}

// deliveryEnv is a core with delivery on a virtual clock, a real store
// and the fake network, driven by the Stepper.
type deliveryEnv struct {
	t    *testing.T
	path string
	cfg  *Config
	clk  *clock.Virtual
	st   *store.Store
	core *Core
	net  *fakeNet
	res  *fakeResolver
	step *Stepper
	seed uint64
}

func newDeliveryEnv(t *testing.T, cfg *Config) *deliveryEnv {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	e := &deliveryEnv{t: t, path: filepath.Join(t.TempDir(), "engine.db"), cfg: cfg,
		clk: clock.NewVirtual(time.UnixMilli(0)), seed: 1,
		res: &fakeResolver{answers: map[string]Resolution{}, lookups: map[string]int{}}}
	e.net = &fakeNet{t: t, clk: e.clk, hosts: map[netip.Addr]*fakeHost{}}
	e.open()
	t.Cleanup(func() { e.st.Close() })
	return e
}

func (e *deliveryEnv) open() {
	e.t.Helper()
	st, err := store.Open(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	core, err := OpenCore(st, e.clk, e.seed)
	if err != nil {
		e.t.Fatal(err)
	}
	core.SetDelivery(e.cfg, clock.Rand(e.seed, "engine.mx"))
	e.st, e.core, e.net.st = st, core, st
	e.step = &Stepper{Core: core, Store: st, Clock: e.clk, Transport: e.net, Resolver: e.res, Helo: e.cfg.Hostname}
}

func (e *deliveryEnv) reopen() {
	e.t.Helper()
	e.st.Close()
	e.open()
}

// host adds a scripted host and its route: domain -> MX host -> addr.
func (e *deliveryEnv) host(domain, name, addr string) *fakeHost {
	h := &fakeHost{name: name}
	e.net.add(h, addr)
	e.res.answers[domain] = Resolution{Status: ResolvedOK, MX: []MXHost{mx(10, name, addr)}}
	return h
}

func (e *deliveryEnv) ingest(tenant, from, to string) string {
	e.t.Helper()
	s := &Submission{Tenant: tenant, From: from, To: to, Subject: "Hello", Text: "body", MsgType: TypeTransactional}
	if err := s.Validate(); err != nil {
		e.t.Fatal(err)
	}
	r := e.core.Ingest(s, "")
	if err := e.st.Apply(e.core.TakeWrites()); err != nil {
		e.t.Fatal(err)
	}
	return r.MsgID
}

// run drives the stepper until nothing is due within d of virtual time.
func (e *deliveryEnv) run(d time.Duration) {
	e.t.Helper()
	if err := e.step.Run(e.clk.Now().Add(d), 1_000_000); err != nil {
		e.t.Fatal(err)
	}
}

func (e *deliveryEnv) events(msgID, kind string) []store.Event {
	e.t.Helper()
	all, err := e.st.Events()
	if err != nil {
		e.t.Fatal(err)
	}
	var out []store.Event
	for _, ev := range all {
		if (msgID == "" || ev.MsgID == msgID) && (kind == "" || ev.Kind == kind) {
			out = append(out, ev)
		}
	}
	return out
}

func (e *deliveryEnv) kinds(msgID string) string {
	var ks []string
	for _, ev := range e.events(msgID, "") {
		ks = append(ks, ev.Kind)
	}
	return strings.Join(ks, ",")
}

func (e *deliveryEnv) state(msgID string) string { return e.core.Message(msgID).State }

func (e *deliveryEnv) allState(want string, ids ...string) {
	e.t.Helper()
	for _, id := range ids {
		if got := e.state(id); got != want {
			e.t.Fatalf("%s is %s, want %s; events %s", id, got, want, e.kinds(id))
		}
	}
}

func bodyHas(raw []byte, parts ...string) error {
	for _, p := range parts {
		if !bytes.Contains(raw, []byte(p)) {
			return fmt.Errorf("missing %q", p)
		}
	}
	return nil
}
