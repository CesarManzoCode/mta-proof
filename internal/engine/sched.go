package engine

import (
	"container/heap"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand/v2"
	"net/netip"
	"sort"
	"strings"

	"github.com/CesarManzoCode/mta-proof/internal/classify"
	"github.com/CesarManzoCode/mta-proof/internal/store"
)

// Delivery actions the core asks its executor to perform (contract 2.6).
const (
	ActResolve = "resolve" // resolve Domain and report it with Resolved
	ActOpen    = "open"    // open connection Conn and report it with Opened
	ActSend    = "send"    // run one transaction on Conn and report it with Sent
	ActClose   = "close"   // QUIT and close Conn and report it with Closed
)

// Action is one thing the executor must do. Before the core returns an
// ActOpen or ActSend, the attempt it starts is among the core's pending
// writes: the executor commits them before touching the network.
type Action struct {
	Kind   string
	Domain string // ActResolve
	Conn   int64
	Open   *OpenSpec // ActOpen
	Send   *SendSpec // ActSend
}

// OpenSpec is where a new connection goes: the ordered MX hosts of the
// route, from one source address.
type OpenSpec struct {
	Src  netip.Addr
	Port int
	MX   []MXHost
}

// SendSpec is the message of one transaction.
type SendSpec struct {
	MsgID string
	From  string
	To    string
	Raw   []byte
}

// Decisions the engine makes without a receiver reply. Their rule ids
// follow the reply rules of 3.8 and are fixed in contract 2.6.
var (
	decNXDomain    = classify.Decision{Class: classify.Permanent, Subtype: classify.General, RuleID: "rfc5321.nxdomain"}
	decMXUnusable  = classify.Decision{Class: classify.Permanent, Subtype: classify.General, RuleID: "rfc5321.mx_unusable"}
	decDNSTemp     = classify.Decision{Class: classify.Transient, Subtype: classify.Connection, RuleID: "rfc5321.dns_temporary"}
	decUnreachable = classify.Decision{Class: classify.Transient, Subtype: classify.Connection, RuleID: "rfc5321.mx_unreachable"}
	decNoReply     = classify.Decision{Class: classify.Transient, Subtype: classify.Connection, RuleID: "engine.no_reply"}
	decSilentClose = classify.Decision{Class: classify.Transient, Subtype: classify.Connection, RuleID: "yahoo.silent_close"}
)

// nullMXReply is the reply the engine records for a null MX destination
// [R-7505 §4.1]; the classifier maps it to permanent/NullMX.
var nullMXReply = Reply{Code: 556, Enhanced: "5.1.10", Text: "Requested action not taken: domain does not accept mail"}

// throttleRules are the provider rules with a transient result: the
// throttling replies of table 3.2.
var throttleRules = func() map[string]bool {
	m := map[string]bool{}
	for _, r := range classify.Default.Rules() {
		if r.Tier == classify.TierProvider && r.Class == classify.Transient {
			m[r.ID] = true
		}
	}
	return m
}()

type queueKey struct{ tenant, domain string }

func (a queueKey) less(b queueKey) bool {
	if a.domain != b.domain {
		return a.domain < b.domain
	}
	return a.tenant < b.tenant
}

type qentry struct {
	at, n int64
	id    string
}

type qheap []qentry

func (h qheap) Len() int { return len(h) }
func (h qheap) Less(i, j int) bool {
	if h[i].at != h[j].at {
		return h[i].at < h[j].at
	}
	return h[i].n < h[j].n
}
func (h qheap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *qheap) Push(x any)   { *h = append(*h, x.(qentry)) }
func (h *qheap) Pop() any {
	old := *h
	e := old[len(old)-1]
	*h = old[:len(old)-1]
	return e
}

// queue is the destination queue of one (tenant, domain): its queued
// messages by (next_at_ms, msg_id sequence). Entries go stale when their
// message leaves the queued state or is rescheduled; head skips them.
type queue struct {
	key queueKey
	h   qheap
}

type route struct {
	status   string
	provider string
	mx       []MXHost
	implicit bool
	err      string
	expires  int64
}

const (
	connOpening = "opening"
	connBusy    = "busy"
	connIdle    = "idle"
	connClosing = "closing"
)

type connState struct {
	id        int64
	q         queueKey
	src       netip.Addr
	provider  string
	lkey      string   // key of the provider scope and of learned limits
	keys      []string // scope keys this connection counts towards
	mx        []MXHost
	mxHost    string
	state     string
	msgs      int // transactions started on this connection
	idleSince int64
	current   string // msg_id of the transaction in flight
}

type learnedLimits struct {
	conns, msgs, rate int
	perMs             int64
}

// scope is one limit scope that applies to a connection or attempt.
type scope struct {
	key     string
	lim     Limit
	learned *learnedLimits // only on the provider scope
}

func (x scope) maxConns() int {
	return minPositive(x.lim.Connections, x.learnedField(func(l *learnedLimits) int { return l.conns }))
}
func (x scope) maxMsgs() int {
	return minPositive(x.lim.MsgsPerConn, x.learnedField(func(l *learnedLimits) int { return l.msgs }))
}

func (x scope) learnedField(f func(*learnedLimits) int) int {
	if x.learned == nil {
		return 0
	}
	return f(x.learned)
}

func minPositive(a, b int) int {
	switch {
	case a <= 0:
		return b
	case b <= 0 || a < b:
		return a
	}
	return b
}

type suppKey struct{ tenant, address string }

// normAddr keeps the local part as given and lower-cases the domain.
func normAddr(a string) string {
	i := strings.LastIndexByte(a, '@')
	if i < 0 {
		return a
	}
	return a[:i+1] + strings.ToLower(a[i+1:])
}

// sched is the delivery state of the core. Queues and suppressions always
// exist; the rest needs SetDelivery.
type sched struct {
	cfg       *Config
	rng       *rand.Rand
	queues    map[queueKey]*queue
	qkeys     []queueKey // sorted
	rr        int
	srcRR     int
	routes    map[string]*route
	resolving map[string]bool
	conns     map[int64]*connState
	nextConn  int64
	starts    map[string][]int64
	learned   map[string]*learnedLimits
	blocked   map[queueKey][]string
	supp      map[suppKey]*store.Suppression
}

func newSched() *sched {
	return &sched{queues: map[queueKey]*queue{}, routes: map[string]*route{}, resolving: map[string]bool{},
		conns: map[int64]*connState{}, starts: map[string][]int64{}, learned: map[string]*learnedLimits{},
		blocked: map[queueKey][]string{}, supp: map[suppKey]*store.Suppression{}}
}

// SetDelivery configures delivery. The MX shuffle draws from its own
// stream of the run seed, so it does not shift the core's other streams.
func (c *Core) SetDelivery(cfg *Config, rng *rand.Rand) {
	c.s.cfg, c.s.rng = cfg, rng
}

func (c *Core) enqueue(m *store.Message) {
	k := queueKey{m.Tenant, m.Domain}
	q := c.s.queues[k]
	if q == nil {
		q = &queue{key: k}
		c.s.queues[k] = q
		i := sort.Search(len(c.s.qkeys), func(i int) bool { return !c.s.qkeys[i].less(k) })
		c.s.qkeys = append(c.s.qkeys, queueKey{})
		copy(c.s.qkeys[i+1:], c.s.qkeys[i:])
		c.s.qkeys[i] = k
	}
	heap.Push(&q.h, qentry{at: m.NextAtMs, n: m.N, id: m.ID})
}

// head returns the first queued message of q, dropping stale entries.
func (c *Core) head(q *queue) *store.Message {
	for len(q.h) > 0 {
		e := q.h[0]
		if m := c.msgs[e.id]; m != nil && m.State == store.StateQueued && m.NextAtMs == e.at {
			return m
		}
		heap.Pop(&q.h)
	}
	return nil
}

func (q *queue) pop() { heap.Pop(&q.h) }

func (c *Core) sortedConns() []*connState {
	out := make([]*connState, 0, len(c.s.conns))
	for _, cs := range c.s.conns {
		out = append(out, cs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func learnKey(domain, provider string, src netip.Addr) string {
	if provider != "" {
		return provider + "/" + src.String()
	}
	return "domain:" + domain + "/" + src.String()
}

// scopes returns the limit scopes of a connection of queue k from src to
// a destination of provider: domain, provider per source IP, tenant and
// source IP (contract 2.6).
func (c *Core) scopes(k queueKey, src netip.Addr, provider string) []scope {
	cfg := c.s.cfg
	lk := learnKey(k.domain, provider, src)
	prov := scope{key: "prov:" + lk, learned: c.s.learned[lk]}
	if provider != "" {
		prov.lim = lookupLimit(cfg.Limits.Provider, provider)
	}
	return []scope{
		{key: "domain:" + k.domain, lim: lookupLimit(cfg.Limits.Domain, k.domain)},
		prov,
		{key: "tenant:" + k.tenant, lim: lookupLimit(cfg.Limits.Tenant, k.tenant)},
		{key: "src:" + src.String(), lim: lookupLimit(cfg.Limits.SrcIP, src.String())},
	}
}

func (c *Core) connCount(key string) int {
	n := 0
	for _, cs := range c.s.conns {
		for _, k := range cs.keys {
			if k == key {
				n++
				break
			}
		}
	}
	return n
}

// windowWait returns 0 if fewer than limit attempts started in the last
// perMs under key, or else the instant the oldest of them leaves the
// window.
func (c *Core) windowWait(key string, limit int, perMs, now int64) int64 {
	if c.windowCount(key, perMs, now) < limit {
		return 0
	}
	list := c.s.starts[key]
	return list[len(list)-limit] + perMs
}

func (c *Core) windowCount(key string, perMs, now int64) int {
	list := c.s.starts[key]
	n := 0
	for i := len(list) - 1; i >= 0 && list[i] > now-perMs; i-- {
		n++
	}
	return n
}

// rateWait returns 0 when every rate of sc allows one more attempt now,
// or else the latest instant one of them frees a slot.
func (c *Core) rateWait(sc []scope, now int64) int64 {
	var w int64
	for _, x := range sc {
		if r := x.lim.Rate; r != nil {
			w = max(w, c.windowWait(x.key, r.Limit, secondsMs(r.PerS), now))
		}
		if l := x.learned; l != nil && l.rate > 0 {
			w = max(w, c.windowWait(x.key, l.rate, l.perMs, now))
		}
	}
	return w
}

func (c *Core) recordStart(keys []string, now int64) {
	keep := now - c.s.cfg.maxWindowMs()
	for _, k := range keys {
		list := append(c.s.starts[k], now)
		i := 0
		for i < len(list) && list[i] <= keep {
			i++
		}
		c.s.starts[k] = list[i:]
	}
}

func scopeKeys(sc []scope) []string {
	out := make([]string, len(sc))
	for i, x := range sc {
		out[i] = x.key
	}
	return out
}

// maxMsgs is the effective transactions-per-connection limit of cs: the
// smallest configured or learned one, 0 for none.
func (c *Core) maxMsgs(cs *connState) int {
	n := 0
	for _, x := range c.scopes(cs.q, cs.src, cs.provider) {
		n = minPositive(n, x.maxMsgs())
	}
	return n
}

func (c *Core) usable(cs *connState) bool {
	n := c.maxMsgs(cs)
	return n == 0 || cs.msgs < n
}

// starving reports whether another queue could not open a connection in
// the last dispatch because of a connection limit cs counts towards; such
// a connection is closed after its transaction instead of being reused,
// so one destination cannot keep a shared limit forever.
func (c *Core) starving(cs *connState) bool {
	for k, keys := range c.s.blocked {
		if k == cs.q {
			continue
		}
		for _, key := range keys {
			for _, own := range cs.keys {
				if key == own {
					return true
				}
			}
		}
	}
	return false
}

// Dispatch serves the destination queues round-robin, one message per
// queue per round, until no queue can make progress: a queue blocked by
// its limits, its route or its schedule does not hold up the others. It
// returns the actions to perform and the next instant something becomes
// due (0 if nothing is scheduled).
func (c *Core) Dispatch() ([]Action, int64) {
	s := c.s
	now := c.nowMs()
	var acts []Action
	var wake int64
	addWake := func(t int64) {
		if t > now && (wake == 0 || t < wake) {
			wake = t
		}
	}
	s.blocked = map[queueKey][]string{}
	for progress := true; progress; {
		progress = false
		for i := 0; i < len(s.qkeys); i++ {
			q := s.queues[s.qkeys[(s.rr+i)%len(s.qkeys)]]
			r := c.serve(q, nil, now)
			acts = append(acts, r.acts...)
			progress = progress || r.progress
			addWake(r.wake)
		}
	}
	if len(s.qkeys) > 0 {
		s.rr = (s.rr + 1) % len(s.qkeys)
	}
	idleMs := secondsMs(s.cfg.IdleS)
	for _, cs := range c.sortedConns() {
		if cs.state != connIdle {
			continue
		}
		if now-cs.idleSince >= idleMs || c.starving(cs) {
			cs.state = connClosing
			acts = append(acts, Action{Kind: ActClose, Conn: cs.id})
		} else {
			addWake(cs.idleSince + idleMs)
		}
	}
	return acts, wake
}

type served struct {
	acts     []Action
	progress bool  // a message left the head of the queue
	wake     int64 // when the queue may progress again, if known
}

// serve handles the head of q: ends it without an attempt when it expired,
// is suppressed or has a route that does not accept mail; otherwise starts
// an attempt on cs (when given), on an idle connection of q or on a new
// connection, within the limits.
func (c *Core) serve(q *queue, cs *connState, now int64) served {
	s := c.s
	m := c.head(q)
	if m == nil {
		return served{}
	}
	if m.NextAtMs > now {
		return served{wake: m.NextAtMs}
	}
	if now >= m.AcceptedAtMs+s.cfg.Retry.LifetimeMs() {
		q.pop()
		c.expireQueued(m)
		return served{progress: true}
	}
	if p := s.supp[suppKey{m.Tenant, normAddr(m.To)}]; p != nil {
		q.pop()
		c.endQueued(m, store.StateSuppressed, &store.Event{Kind: KindSuppressed, Class: classify.Permanent,
			Subtype: p.Subtype, Detail: detail(map[string]any{"suppressed_by": p.MsgID})})
		return served{progress: true}
	}
	rt := s.routes[m.Domain]
	if rt == nil || (cs == nil && rt.expires <= now) {
		if s.resolving[m.Domain] {
			return served{}
		}
		s.resolving[m.Domain] = true
		return served{acts: []Action{{Kind: ActResolve, Domain: m.Domain}}}
	}
	switch rt.status {
	case ResolvedNullMX:
		q.pop()
		c.bounceNullMX(m)
		return served{progress: true}
	case ResolvedNXDomain:
		q.pop()
		c.bounceRoute(m, rt, decNXDomain)
		return served{progress: true}
	case ResolvedUnusable:
		q.pop()
		c.bounceRoute(m, rt, decMXUnusable)
		return served{progress: true}
	}
	if cs != nil {
		if w := c.rateWait(c.scopes(cs.q, cs.src, cs.provider), now); w > 0 {
			return served{wake: w}
		}
		q.pop()
		return served{acts: []Action{c.assign(cs, m, now)}, progress: true}
	}
	var wake int64
	for _, idle := range c.sortedConns() {
		if idle.q != q.key || idle.state != connIdle || !c.usable(idle) {
			continue
		}
		if w := c.rateWait(c.scopes(idle.q, idle.src, idle.provider), now); w > 0 {
			if wake == 0 || w < wake {
				wake = w
			}
			continue
		}
		q.pop()
		return served{acts: []Action{c.assign(idle, m, now)}, progress: true}
	}
	src, sc, w := c.pickSrc(q.key, rt.provider, now)
	if !src.IsValid() {
		if w > 0 && (wake == 0 || w < wake) {
			wake = w
		}
		return served{wake: wake}
	}
	q.pop()
	s.nextConn++
	nc := &connState{id: s.nextConn, q: q.key, src: src, provider: rt.provider,
		lkey: learnKey(q.key.domain, rt.provider, src), keys: scopeKeys(sc), mx: rt.mx,
		state: connOpening, current: m.ID}
	s.conns[nc.id] = nc
	c.StartAttempt(m.ID, Route{Provider: rt.provider, SrcIP: src.String()})
	c.recordStart(nc.keys, now)
	return served{progress: true, acts: []Action{{Kind: ActOpen, Conn: nc.id,
		Open: &OpenSpec{Src: src, Port: s.cfg.SMTPPort, MX: rt.mx}}}}
}

// pickSrc returns the first source address, round-robin, whose
// connection limits and rates allow one more connection of queue k. When
// none does, it records the connection limits that blocked k and returns
// the earliest instant a rate frees up.
func (c *Core) pickSrc(k queueKey, provider string, now int64) (netip.Addr, []scope, int64) {
	ips := c.s.cfg.srcIPs
	var blocked []string
	var wake int64
	for i := range ips {
		src := ips[(c.s.srcRR+i)%len(ips)]
		sc := c.scopes(k, src, provider)
		ok := true
		for _, x := range sc {
			if n := x.maxConns(); n > 0 && c.connCount(x.key) >= n {
				blocked = append(blocked, x.key)
				ok = false
			}
		}
		if !ok {
			continue
		}
		if w := c.rateWait(sc, now); w > 0 {
			if wake == 0 || w < wake {
				wake = w
			}
			continue
		}
		c.s.srcRR = (c.s.srcRR + i + 1) % len(ips)
		return src, sc, 0
	}
	if len(blocked) > 0 {
		c.s.blocked[k] = blocked
	}
	return netip.Addr{}, nil, wake
}

// assign starts the attempt of m on the open connection cs.
func (c *Core) assign(cs *connState, m *store.Message, now int64) Action {
	cs.state, cs.current = connBusy, m.ID
	cs.msgs++
	r := Route{Provider: cs.provider, MXHost: cs.mxHost, SrcIP: cs.src.String()}
	c.StartAttempt(m.ID, r)
	c.recordStart(cs.keys, now)
	c.event(&store.Event{Kind: KindRouted, Provider: r.Provider, MXHost: r.MXHost, SrcIP: r.SrcIP,
		Attempt: m.Attempts, Detail: detail(map[string]any{"conn": cs.id, "reused": true})}, m)
	return Action{Kind: ActSend, Conn: cs.id, Send: &SendSpec{MsgID: m.ID, From: m.From, To: m.To, Raw: m.Raw}}
}

// Resolved records the route of domain. A temporary failure defers every
// due message of the domain and forgets the route, so the next due message
// resolves again [R-5321 §5.1].
func (c *Core) Resolved(domain string, res Resolution) {
	s := c.s
	delete(s.resolving, domain)
	now := c.nowMs()
	ttl := s.cfg.RouteTTLS * 1000
	if res.Status == ResolvedOK && len(res.MX) == 0 {
		res = Resolution{Status: ResolvedUnusable, Implicit: res.Implicit, Err: "no MX host with an address"}
	}
	switch res.Status {
	case ResolvedTemporary:
		delete(s.routes, domain)
		for _, k := range s.qkeys {
			if k.domain != domain {
				continue
			}
			q := s.queues[k]
			for m := c.head(q); m != nil && m.NextAtMs <= now; m = c.head(q) {
				q.pop()
				c.deferQueued(m, decDNSTemp, map[string]any{"dns": res.Err})
			}
		}
	case ResolvedOK:
		mx := orderMX(res.MX, s.rng.Shuffle)
		s.routes[domain] = &route{status: ResolvedOK, provider: s.cfg.providerOf(mx[0].Host), mx: mx,
			implicit: res.Implicit, expires: now + ttl}
	default:
		s.routes[domain] = &route{status: res.Status, err: res.Err, implicit: res.Implicit, expires: now + ttl}
	}
}

func mxNames(mx []MXHost) []string {
	out := make([]string, len(mx))
	for i, h := range mx {
		out[i] = h.Host
	}
	return out
}

// Opened records the outcome of opening connection id for the message
// that started it. On success it returns the send of that message; on
// failure the connection is gone and the message's attempt is finished.
func (c *Core) Opened(id int64, res OpenResult) []Action {
	s := c.s
	cs := s.conns[id]
	if cs == nil {
		return nil
	}
	m := c.msgs[cs.current]
	r := Route{Provider: cs.provider, MXHost: res.MXHost, SrcIP: cs.src.String()}
	d := map[string]any{"conn": id, "mx": mxNames(cs.mx)}
	if len(res.Tried) > 0 {
		d["tried"] = res.Tried
	}
	if !res.OK && res.Reply == nil {
		delete(s.conns, id)
		c.deferInflight(m, r, nil, decUnreachable, true, d)
		return nil
	}
	if res.Addr != "" {
		d["addr"] = res.Addr
	}
	c.event(&store.Event{Kind: KindRouted, Provider: r.Provider, MXHost: r.MXHost, SrcIP: r.SrcIP,
		Attempt: m.Attempts, Detail: detail(d)}, m)
	if !res.OK {
		cs.mxHost = res.MXHost
		c.replyFailure(cs, m, r, *res.Reply, res.Stage)
		delete(s.conns, id)
		return nil
	}
	cs.mxHost, cs.state = res.MXHost, connBusy
	cs.msgs = 1
	return []Action{{Kind: ActSend, Conn: id, Send: &SendSpec{MsgID: m.ID, From: m.From, To: m.To, Raw: m.Raw}}}
}

// Sent records the outcome of the transaction of msgID on connection id
// and returns what the connection does next: another message of its queue,
// a close, or nothing (idle). The outcome is among the pending writes, so
// it is committed before the executor acts on the returned actions.
func (c *Core) Sent(id int64, msgID string, tr TxResult) []Action {
	s := c.s
	cs := s.conns[id]
	m := c.msgs[msgID]
	if cs == nil || m == nil {
		return nil
	}
	now := c.nowMs()
	r := Route{Provider: cs.provider, MXHost: cs.mxHost, SrcIP: cs.src.String()}
	closeConn := tr.Closed
	switch {
	case tr.Reply != nil && tr.Stage == "data_end" && tr.Reply.Code/100 == 2:
		c.FinishAttempt(m.ID, Outcome{Kind: KindDelivered, Route: r, Code: tr.Reply.Code,
			Enhanced: tr.Reply.Enhanced, Text: tr.Reply.Text, Detail: map[string]any{"conn": id}})
	case tr.Reply != nil:
		if c.replyFailure(cs, m, r, *tr.Reply, tr.Stage) {
			closeConn = true
		}
	case tr.Stage == "mail" && cs.msgs > 1:
		// The receiver closed without a reply when this connection's next
		// message began: the closing of R-YAHOO at its per-connection
		// limit. The limit is the transactions it took; the message goes
		// back to the queue at once, since reconnecting immediately is
		// allowed, and the retry schedule does not advance.
		learned := c.learnMsgsPerConn(cs.lkey, cs.msgs-1)
		c.deferInflight(m, r, nil, decSilentClose, false,
			map[string]any{"conn": id, "stage": tr.Stage, "error": tr.Err, "learned": learned})
		closeConn = true
	default:
		c.deferInflight(m, r, nil, decNoReply, true, map[string]any{"conn": id, "stage": tr.Stage, "error": tr.Err})
		closeConn = true
	}
	cs.current = ""
	if closeConn || !c.usable(cs) || c.starving(cs) {
		cs.state = connClosing
		return []Action{{Kind: ActClose, Conn: id}}
	}
	cs.state, cs.idleSince = connIdle, now
	q := s.queues[cs.q]
	for {
		r := c.serve(q, cs, now)
		if len(r.acts) > 0 || !r.progress {
			return r.acts
		}
	}
}

// Closed forgets connection id once the executor has closed it.
func (c *Core) Closed(id int64) { delete(c.s.conns, id) }

// replyFailure classifies a refusal of m's attempt and finishes the
// attempt: permanent bounces (and a NoEmail suppresses the address);
// transient and undetermined (D14) defer or expire. A throttling reply
// lowers the learned limits of the connection's provider scope. It
// reports whether the connection must be closed.
func (c *Core) replyFailure(cs *connState, m *store.Message, r Route, rep Reply, stage string) bool {
	d, _ := c.Classify(m.ID, r, rep.Code, rep.Enhanced, rep.Text)
	det := map[string]any{"conn": cs.id, "stage": stage}
	if d.Class == classify.Permanent {
		if d.Subtype == classify.NoEmail {
			c.suppress(m, d.Subtype)
			det["suppressed"] = true
		}
		c.FinishAttempt(m.ID, Outcome{Kind: KindBounced, Route: r, Code: rep.Code, Enhanced: rep.Enhanced,
			Text: rep.Text, Class: d.Class, Subtype: d.Subtype, RuleID: d.RuleID, Detail: det})
		return rep.Code == 421
	}
	throttle := throttleRules[d.RuleID]
	if throttle {
		det["learned"] = c.learnThrottle(cs)
	}
	c.deferInflight(m, r, &rep, d, true, det)
	return throttle || rep.Code == 421
}

// deferInflight finishes m's attempt as deferred. A counted deferral
// advances the retry schedule (contract 5) and expires the message when
// its next attempt would fall after its lifetime; an uncounted one puts it
// back at once.
func (c *Core) deferInflight(m *store.Message, r Route, rep *Reply, d classify.Decision, counted bool, det map[string]any) {
	o := Outcome{Kind: KindDeferred, Route: r, Class: d.Class, Subtype: d.Subtype, RuleID: d.RuleID, Detail: det}
	if rep != nil {
		o.Code, o.Enhanced, o.Text = rep.Code, rep.Enhanced, rep.Text
	}
	now := c.nowMs()
	o.NextAtMs = now
	if counted {
		k := m.Retries + 1
		next := now + c.s.cfg.Retry.DelayMs(k)
		if next > m.AcceptedAtMs+c.s.cfg.Retry.LifetimeMs() {
			o.Kind = KindExpired
			c.FinishAttempt(m.ID, o)
			c.enqueueDSN(m, rep, r.MXHost, now)
			return
		}
		m.Retries, o.NextAtMs = k, next
	}
	c.FinishAttempt(m.ID, o)
}

// deferQueued defers a queued message that failed before any attempt (a
// temporary DNS error), with the same schedule and expiry as an attempt.
func (c *Core) deferQueued(m *store.Message, d classify.Decision, det map[string]any) {
	now := c.nowMs()
	k := m.Retries + 1
	next := now + c.s.cfg.Retry.DelayMs(k)
	ev := &store.Event{Class: d.Class, Subtype: d.Subtype, RuleID: d.RuleID, Attempt: m.Attempts, Detail: detail(det)}
	if next > m.AcceptedAtMs+c.s.cfg.Retry.LifetimeMs() {
		ev.Kind = KindExpired
		c.endQueued(m, store.StateExpired, ev)
		c.enqueueDSN(m, nil, "", 0)
		return
	}
	m.Retries, m.NextAtMs, m.UpdatedAtMs = k, next, now
	c.update(m)
	ev.Kind = KindDeferred
	c.event(ev, m)
	c.enqueue(m)
}

// endQueued moves a queued message to a terminal state without an attempt.
func (c *Core) endQueued(m *store.Message, state string, ev *store.Event) {
	m.State, m.UpdatedAtMs = state, c.nowMs()
	c.update(m)
	if ev.Attempt == 0 {
		ev.Attempt = m.Attempts
	}
	c.event(ev, m)
}

func (c *Core) expireQueued(m *store.Message) {
	c.endQueued(m, store.StateExpired, &store.Event{Kind: KindExpired,
		Detail: detail(map[string]any{"reason": "lifetime"})})
	c.enqueueDSN(m, nil, "", 0)
}

// bounceNullMX ends a message to a null MX domain without connecting or
// retrying, with the reply R-7505 §4.1 prescribes.
func (c *Core) bounceNullMX(m *store.Message) {
	rep := nullMXReply
	d := classify.Classify(classify.Reply{Code: rep.Code, Enhanced: rep.Enhanced, Text: rep.Text})
	c.event(&store.Event{Kind: KindRouted, Attempt: m.Attempts, Detail: detail(map[string]any{"null_mx": true})}, m)
	c.event(&store.Event{Kind: KindClassified, Attempt: m.Attempts, Code: rep.Code, Enhanced: rep.Enhanced,
		Text: rep.Text, Class: d.Class, Subtype: d.Subtype, RuleID: d.RuleID}, m)
	c.endQueued(m, store.StateBounced, &store.Event{Kind: KindBounced, Code: rep.Code, Enhanced: rep.Enhanced,
		Text: rep.Text, Class: d.Class, Subtype: d.Subtype, RuleID: d.RuleID})
}

// bounceRoute ends a message whose domain does not exist or has no usable
// host [R-5321 §5.1].
func (c *Core) bounceRoute(m *store.Message, rt *route, d classify.Decision) {
	det := detail(map[string]any{"dns": rt.err})
	c.event(&store.Event{Kind: KindClassified, Attempt: m.Attempts, Class: d.Class, Subtype: d.Subtype,
		RuleID: d.RuleID, Detail: det}, m)
	c.endQueued(m, store.StateBounced, &store.Event{Kind: KindBounced, Class: d.Class, Subtype: d.Subtype,
		RuleID: d.RuleID, Detail: det})
}

func (c *Core) suppress(m *store.Message, subtype string) {
	k := suppKey{m.Tenant, normAddr(m.To)}
	if c.s.supp[k] != nil {
		return
	}
	p := &store.Suppression{Tenant: k.tenant, Address: k.address, MsgID: m.ID, Subtype: subtype, CreatedAtMs: c.nowMs()}
	c.s.supp[k] = p
	c.writes = append(c.writes, store.Write{PutSuppression: p})
}

// learnMsgsPerConn lowers the learned transactions-per-connection limit of
// a provider scope and returns what it learned for the event log.
func (c *Core) learnMsgsPerConn(lk string, n int) map[string]any {
	l := c.learnedFor(lk)
	if n < 1 {
		n = 1
	}
	if l.msgs == 0 || n < l.msgs {
		l.msgs = n
	}
	return map[string]any{"key": lk, "msgs_per_conn": l.msgs}
}

// learnThrottle halves the learned rate of cs's provider scope (from the
// attempts started in the last adaptation window) and lowers its learned
// connection limit to one less than the connections open (supuesto: the
// factors are the project's, no source publishes them).
func (c *Core) learnThrottle(cs *connState) map[string]any {
	l := c.learnedFor(cs.lkey)
	key := "prov:" + cs.lkey
	per := secondsMs(c.s.cfg.Adapt.RateWindowS)
	rate := max(1, c.windowCount(key, per, c.nowMs())/2)
	if l.rate == 0 || rate < l.rate {
		l.rate, l.perMs = rate, per
	}
	open := c.connCount(key)
	if c.s.conns[cs.id] == nil {
		open++
	}
	if n := max(1, open-1); l.conns == 0 || n < l.conns {
		l.conns = n
	}
	return map[string]any{"key": cs.lkey, "rate_limit": l.rate, "rate_per_s": c.s.cfg.Adapt.RateWindowS,
		"connections": l.conns}
}

// restoreLearned applies the learned limits recorded in the detail of a
// deferred or expired event, keeping the lowest value of each, as learning
// does. Start-up calls it for every such event in seq order, so a restart
// keeps what the engine learned before it.
func (c *Core) restoreLearned(detail string) {
	var d struct {
		Learned *struct {
			Key         string  `json:"key"`
			MsgsPerConn int     `json:"msgs_per_conn"`
			RateLimit   int     `json:"rate_limit"`
			RatePerS    float64 `json:"rate_per_s"`
			Connections int     `json:"connections"`
		} `json:"learned"`
	}
	if json.Unmarshal([]byte(detail), &d) != nil || d.Learned == nil || d.Learned.Key == "" {
		return
	}
	in := d.Learned
	l := c.learnedFor(in.Key)
	if n := in.MsgsPerConn; n > 0 && (l.msgs == 0 || n < l.msgs) {
		l.msgs = n
	}
	if n := in.RateLimit; n > 0 && in.RatePerS > 0 && (l.rate == 0 || n < l.rate) {
		l.rate, l.perMs = n, secondsMs(in.RatePerS)
	}
	if n := in.Connections; n > 0 && (l.conns == 0 || n < l.conns) {
		l.conns = n
	}
}

func (c *Core) learnedFor(lk string) *learnedLimits {
	l := c.s.learned[lk]
	if l == nil {
		l = &learnedLimits{}
		c.s.learned[lk] = l
	}
	return l
}

// enqueueDSN queues the delivery status notification of an expired
// message: a new message with a null reverse-path to the original
// reverse-path [R-5321 §4.5.5, R-3464 §2]. A message that itself had a
// null reverse-path gets none.
func (c *Core) enqueueDSN(orig *store.Message, last *Reply, remote string, lastAttemptMs int64) {
	if orig.From == "" {
		return
	}
	now := c.nowMs()
	c.lastN++
	id := msgIDFor(c.lastN)
	raw := BuildDSN(DSN{ID: id, Orig: orig, Hostname: c.s.cfg.Hostname, Postmaster: c.s.cfg.Postmaster,
		At: c.clk.Now(), Last: last, RemoteMTA: remote, LastAttemptMs: lastAttemptMs})
	sum := sha256.Sum256(raw)
	m := &store.Message{N: c.lastN, ID: id, Tenant: orig.Tenant, From: "", To: orig.From,
		Domain: domainOf(orig.From), MsgType: orig.MsgType, PayloadSHA256: hex.EncodeToString(sum[:]), Raw: raw,
		State: store.StateQueued, NextAtMs: now, AcceptedAtMs: now, UpdatedAtMs: now}
	c.msgs[id] = m
	ins := *m
	c.writes = append(c.writes, store.Write{InsertMessage: &ins})
	c.event(&store.Event{Kind: KindAccepted, Detail: detail(map[string]any{"dsn_for": orig.ID,
		"payload_sha256": m.PayloadSHA256})}, m)
	c.event(&store.Event{Kind: KindDSNEnqueued, Detail: detail(map[string]any{"dsn_msg_id": id})}, orig)
	c.enqueue(m)
}
