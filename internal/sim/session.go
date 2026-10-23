package sim

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Clock is the time source of the receiver: the wall clock in mtasim, a
// virtual clock in mtadst.
type Clock interface {
	Now() time.Time
}

// WallClock reads the system clock.
type WallClock struct{}

// Now returns time.Now.
func (WallClock) Now() time.Time { return time.Now() }

// Config builds a Receiver.
type Config struct {
	// Dir receives receiver.jsonl and messages/.
	Dir      string
	Profiles map[string]*Profile
	Clock    Clock
	// NoSync skips the fsync of each data_end and stored message (for
	// in-process runs whose durability does not matter).
	NoSync bool
	// StartTLS offers the STARTTLS extension [R-3207] on every host; the
	// transport runs the handshake.
	StartTLS bool
}

// Receiver holds the state shared by all sessions: profiles, the host to
// profile map, per-profile counters, the log and the message store. It is
// safe for concurrent use; mtadst drives it from a single goroutine.
type Receiver struct {
	mu       sync.Mutex
	clock    Clock
	log      *Log
	store    *Store
	profiles map[string]*Profile
	hosts    map[string]string
	state    map[string]*profileState
	nextConn int64
	startTLS bool
}

type windowKey struct {
	rule *compiledRule
	key  string
}

type profileState struct {
	firstConn map[netip.Addr]int64
	open      map[netip.Addr]int
	windows   map[windowKey][]int64
	dataSeen  map[*compiledRule]int
}

// NewReceiver creates the log and message store in cfg.Dir and maps each
// profile host to its profile.
func NewReceiver(cfg Config) (*Receiver, error) {
	if cfg.Clock == nil {
		cfg.Clock = WallClock{}
	}
	if len(cfg.Profiles) == 0 {
		return nil, errors.New("no profiles")
	}
	r := &Receiver{
		clock:    cfg.Clock,
		profiles: cfg.Profiles,
		hosts:    map[string]string{},
		state:    map[string]*profileState{},
		startTLS: cfg.StartTLS,
	}
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, h := range cfg.Profiles[name].Hosts {
			h = normHost(h)
			if other, ok := r.hosts[h]; ok {
				return nil, fmt.Errorf("host %s is declared by %s and %s", h, other, name)
			}
			r.hosts[h] = name
		}
	}
	store, err := NewStore(filepath.Join(cfg.Dir, "messages"), !cfg.NoSync)
	if err != nil {
		return nil, err
	}
	log, err := CreateLog(filepath.Join(cfg.Dir, "receiver.jsonl"), !cfg.NoSync)
	if err != nil {
		return nil, err
	}
	r.store, r.log = store, log
	return r, nil
}

// Close closes the log.
func (r *Receiver) Close() error { return r.log.Close() }

// Hosts returns the served host names, sorted.
func (r *Receiver) Hosts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.hosts))
	for h := range r.hosts {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// ProfileOf returns the active profile of a host.
func (r *Receiver) ProfileOf(host string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hosts[normHost(host)]
}

// SwitchProfile makes new connections to host use profile. Open sessions
// keep the profile they started with.
func (r *Receiver) SwitchProfile(host, profile string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	host = normHost(host)
	if _, ok := r.hosts[host]; !ok {
		return fmt.Errorf("unknown host %q", host)
	}
	if _, ok := r.profiles[profile]; !ok {
		return fmt.Errorf("unknown profile %q", profile)
	}
	r.hosts[host] = profile
	return nil
}

// SwitchFrom moves every host whose active profile is from to profile and
// returns how many hosts changed.
func (r *Receiver) SwitchFrom(from, profile string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.profiles[profile]; !ok {
		return 0, fmt.Errorf("unknown profile %q", profile)
	}
	n := 0
	for h, p := range r.hosts {
		if p == from {
			r.hosts[h] = profile
			n++
		}
	}
	return n, nil
}

func (r *Receiver) stateOf(profile string) *profileState {
	st := r.state[profile]
	if st == nil {
		st = &profileState{
			firstConn: map[netip.Addr]int64{},
			open:      map[netip.Addr]int{},
			windows:   map[windowKey][]int64{},
			dataSeen:  map[*compiledRule]int{},
		}
		r.state[profile] = st
	}
	return st
}

func normHost(h string) string { return strings.TrimSuffix(strings.ToLower(h), ".") }

// Result is what the transport must do after feeding the session an event.
type Result struct {
	// Reply is the final reply line; nil means send nothing.
	Reply *Response
	// Ext are the lines of a multi-line reply sent before Reply, each as
	// "<code>-<ext>".
	Ext []string
	// Close asks the transport to close the connection after the reply
	// (or silently when Reply is nil).
	Close bool
	// Delay is the profile latency to wait before sending the reply.
	Delay time.Duration
	// DropMidData is set on the 354 reply when a drop_mid_data rule fired:
	// the transport reads at most DropAfter bytes of message data and then
	// calls DropData.
	DropMidData bool
	DropAfter   int
	// MsgID and Accepted describe the reply to DATA (data_end).
	MsgID    string
	Accepted bool
	// StartTLS is set on the 220 reply to STARTTLS: the transport runs
	// the TLS handshake right after sending it and reports it with
	// TLSStarted or TLSFailed.
	StartTLS bool
}

// Wire renders the reply as it goes on the connection.
func (res Result) Wire() []byte {
	if res.Reply == nil {
		return nil
	}
	var b bytes.Buffer
	for _, e := range res.Ext {
		fmt.Fprintf(&b, "%d-%s\r\n", res.Reply.Code, e)
	}
	b.WriteString(res.Reply.String())
	b.WriteString("\r\n")
	return b.Bytes()
}

// Session is the SMTP state machine of one connection. It knows nothing
// about sockets: the transport feeds it command lines and message data and
// writes the replies it returns.
type Session struct {
	r       *Receiver
	id      int64
	host    string
	profile *Profile
	st      *profileState
	src     netip.Addr
	dst     netip.Addr

	greeted  bool
	inTx     bool
	mailFrom string
	rcpts    []string
	msgs     int
	inData   bool
	dropRule string
	ended    bool
	tls      *TLS // the session runs over TLS
}

// ErrUnknownHost is returned by Open for a host no profile serves.
var ErrUnknownHost = errors.New("unknown host")

// Open starts a session for a connection from src to dst on host and
// returns the greeting (or the reply of a connection-time rule).
func (r *Receiver) Open(host string, src, dst netip.Addr) (*Session, Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	host = normHost(host)
	name, ok := r.hosts[host]
	if !ok {
		return nil, Result{}, fmt.Errorf("%w %q", ErrUnknownHost, host)
	}
	r.nextConn++
	s := &Session{
		r: r, id: r.nextConn, host: host, profile: r.profiles[name],
		st: r.stateOf(name), src: src.Unmap(), dst: dst.Unmap(),
	}
	now := r.nowMs()
	if _, seen := s.st.firstConn[s.src]; !seen {
		s.st.firstConn[s.src] = now
	}
	s.emit(Event{Ev: EvConnect})
	s.st.open[s.src]++
	for _, c := range s.profile.compiled {
		if c.kind == KindConcurrency && c.appliesTo(s.src) && s.st.open[s.src]-1 >= c.limit {
			return s, s.violate(c, StageConnect), nil
		}
	}
	if res, ok := s.respondRule(StageConnect, ""); ok {
		return s, res, nil
	}
	return s, s.reply(Response{Code: 220, Text: host + " ESMTP mtasim"}, StageConnect), nil
}

func (r *Receiver) nowMs() int64 { return r.clock.Now().UnixMilli() }

// ID is the connection number written as conn in the log.
func (s *Session) ID() int64 { return s.id }

// Host returns the host the session is connected to.
func (s *Session) Host() string { return s.host }

// SrcIP returns the client address.
func (s *Session) SrcIP() netip.Addr { return s.src }

// Line feeds one command line (without CRLF).
func (s *Session) Line(line string) Result {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if s.ended {
		return Result{Close: true}
	}
	verb, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	switch strings.ToUpper(verb) {
	case "EHLO", "HELO":
		return s.cmdHello(strings.ToUpper(verb), arg)
	case "MAIL":
		return s.cmdMail(arg)
	case "RCPT":
		return s.cmdRcpt(arg)
	case "DATA":
		return s.cmdData()
	case "RSET":
		s.resetTx()
		return s.reply(Response{250, "2.0.0", "OK"}, StageAny)
	case "NOOP":
		return s.reply(Response{250, "2.0.0", "OK"}, StageAny)
	case "QUIT":
		res := s.reply(Response{221, "2.0.0", s.host + " closing connection"}, StageAny)
		s.end(EvClose, "")
		res.Close = true
		return res
	case "VRFY":
		return s.reply(Response{252, "2.5.0", "Cannot VRFY user, but will accept message and attempt delivery"}, StageAny)
	case "STARTTLS":
		return s.cmdStartTLS(arg)
	case "EXPN", "HELP", "AUTH", "BDAT":
		return s.reply(Response{502, "5.5.1", "Command not implemented"}, StageAny)
	default:
		return s.reply(Response{500, "5.5.2", "Syntax error, command unrecognized"}, StageAny)
	}
}

func (s *Session) cmdHello(verb, arg string) Result {
	if arg == "" {
		return s.reply(Response{501, "5.5.4", "Syntax error in parameters or arguments"}, StageAny)
	}
	s.emit(Event{Ev: EvEHLO})
	// RFC 5321 §4.1.4: a later EHLO resets the state like RSET.
	s.resetTx()
	s.greeted = true
	if verb == "HELO" {
		return s.reply(Response{Code: 250, Text: s.host}, StageEHLO)
	}
	res := s.reply(Response{Code: 250, Text: "ENHANCEDSTATUSCODES"}, StageEHLO)
	res.Ext = []string{s.host + " greets " + arg, "8BITMIME"}
	// R-3207 §4.2: STARTTLS is not offered again once TLS is active.
	if s.r.startTLS && s.tls == nil {
		res.Ext = append(res.Ext, "STARTTLS")
	}
	return res
}

// cmdStartTLS answers STARTTLS [R-3207 §4]: 220 when offered, with the
// handshake left to the transport.
func (s *Session) cmdStartTLS(arg string) Result {
	switch {
	case !s.r.startTLS:
		return s.reply(Response{502, "5.5.1", "Command not implemented"}, StageAny)
	case arg != "":
		return s.reply(Response{501, "5.5.4", "Syntax error (no parameters allowed)"}, StageAny)
	case s.tls != nil || !s.greeted || s.inTx:
		return s.reply(Response{503, "5.5.1", "Bad sequence of commands"}, StageAny)
	}
	res := s.reply(Response{220, "2.0.0", "Ready to start TLS"}, StageAny)
	res.StartTLS = true
	return res
}

// TLSStarted records a completed handshake. The SMTP state goes back to
// the one after the greeting and what the client said before is
// forgotten [R-3207 §4.2]: the client must send EHLO again.
func (s *Session) TLSStarted(version, cipher string) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if s.ended {
		return
	}
	s.tls = &TLS{Version: version, Cipher: cipher}
	s.emit(Event{Ev: EvStartTLS, TLS: s.tls})
	s.resetTx()
	s.greeted = false
}

// TLSFailed records a handshake that failed; the connection is closed.
func (s *Session) TLSFailed(err error) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if s.ended {
		return
	}
	s.emit(Event{Ev: EvStartTLS, TLS: &TLS{Error: err.Error()}})
	s.end(EvClose, "")
}

func (s *Session) cmdMail(arg string) Result {
	if !s.greeted {
		return s.reply(Response{503, "5.5.1", "Bad sequence of commands: send EHLO first"}, StageAny)
	}
	if s.inTx {
		return s.reply(Response{503, "5.5.1", "Bad sequence of commands: nested MAIL command"}, StageAny)
	}
	from, ok := parsePath(arg, "FROM:")
	if !ok {
		return s.reply(Response{501, "5.5.4", "Syntax error in parameters or arguments"}, StageAny)
	}
	s.emit(Event{Ev: EvMail, MailFrom: &from})
	s.mailFrom = from
	now := s.r.nowMs()
	for _, c := range s.profile.compiled {
		if !c.appliesTo(s.src) {
			continue
		}
		switch {
		case c.kind == KindMaxMsgsPerConn && s.msgs >= c.limit:
			return s.violate(c, StageMail)
		case c.kind == KindRate && c.rate.Scope != ScopeRcpt && s.windowFull(c, s.rateKey(c, ""), now):
			return s.violate(c, StageMail)
		}
	}
	if res, ok := s.respondRule(StageMail, ""); ok {
		return res
	}
	for _, c := range s.profile.compiled {
		if c.kind == KindRate && c.rate.Scope != ScopeRcpt && c.appliesTo(s.src) {
			s.record(c, s.rateKey(c, ""), now)
		}
	}
	s.msgs++
	s.inTx = true
	return s.reply(Response{250, "2.1.0", "Sender OK"}, StageMail)
}

func (s *Session) cmdRcpt(arg string) Result {
	if !s.inTx {
		return s.reply(Response{503, "5.5.1", "Bad sequence of commands: need MAIL first"}, StageAny)
	}
	rcpt, ok := parsePath(arg, "TO:")
	if !ok || rcpt == "" {
		return s.reply(Response{501, "5.5.4", "Syntax error in parameters or arguments"}, StageAny)
	}
	s.emit(Event{Ev: EvRcpt, Rcpt: rcpt})
	now := s.r.nowMs()
	for _, c := range s.profile.compiled {
		if c.kind == KindRate && c.rate.Scope == ScopeRcpt && c.appliesTo(s.src) && s.windowFull(c, s.rateKey(c, rcpt), now) {
			return s.violate(c, StageRcpt)
		}
	}
	if res, ok := s.respondRule(StageRcpt, rcpt); ok {
		return res
	}
	for _, c := range s.profile.compiled {
		if c.kind == KindRate && c.rate.Scope == ScopeRcpt && c.appliesTo(s.src) {
			s.record(c, s.rateKey(c, rcpt), now)
		}
	}
	s.rcpts = append(s.rcpts, rcpt)
	return s.reply(Response{250, "2.1.5", "Recipient OK"}, StageRcpt)
}

func (s *Session) cmdData() Result {
	if !s.inTx || len(s.rcpts) == 0 {
		return s.reply(Response{503, "5.5.1", "Bad sequence of commands: need RCPT first"}, StageAny)
	}
	s.inData = true
	s.dropRule = ""
	res := s.reply(Response{Code: 354, Text: "Start mail input; end with <CRLF>.<CRLF>"}, StageData)
	for _, c := range s.profile.compiled {
		if c.kind != KindDropMidData {
			continue
		}
		s.st.dataSeen[c]++
		if s.st.dataSeen[c]%c.drop.EveryN == 0 {
			s.dropRule = c.id
			res.DropMidData, res.DropAfter = true, c.drop.AfterBytes
			break
		}
	}
	return res
}

// DropData ends a DATA phase cut short by a drop_mid_data rule: the
// connection is closed without a reply.
func (s *Session) DropData() Result {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if !s.ended {
		s.end(EvDrop, s.dropRule)
	}
	return Result{Close: true}
}

// Data feeds the complete message data (dot-unstuffed, without the final
// "."), stores it and returns the final reply to DATA. The data_end line is
// synced before Data returns, so the transport may send the reply at once.
func (s *Session) Data(msg []byte) Result {
	sum, storeErr := s.r.store.Put(msg)
	msgID := headerValue(msg, "X-Mtaproof-Id")

	s.r.mu.Lock()
	if s.ended || !s.inData {
		s.r.mu.Unlock()
		return Result{Close: s.ended}
	}
	resp, closeAfter := Response{250, "2.0.0", "OK"}, false
	if storeErr != nil {
		resp, sum = Response{451, "4.3.0", "Requested action aborted: local error in processing"}, ""
	} else if c, rcptOK := s.matchRespond(StageDataEnd, ""); c != nil && rcptOK {
		resp, closeAfter = render(c.respond.Response, domainOf(s.mailFrom)), c.respond.Close
	}
	res := s.finishData(resp, msgID, sum)
	if closeAfter {
		s.end(EvClose, "")
		res.Close = true
	}
	s.r.mu.Unlock()
	if err := s.r.log.Sync(); err != nil {
		// Without a durable data_end the acceptance cannot be claimed.
		return Result{Close: true}
	}
	return res
}

// DataOversize ends a DATA phase whose message exceeded the transport's
// size cap (RFC 5321 §4.5.3.1.9, "552 Too much mail data").
func (s *Session) DataOversize() Result {
	s.r.mu.Lock()
	if s.ended || !s.inData {
		s.r.mu.Unlock()
		return Result{Close: s.ended}
	}
	res := s.finishData(Response{552, "5.3.4", "Too much mail data."}, "", "")
	s.r.mu.Unlock()
	s.r.log.Sync()
	return res
}

func (s *Session) finishData(resp Response, msgID, sum string) Result {
	from := s.mailFrom
	s.emit(Event{
		Ev: EvDataEnd, MailFrom: &from, Rcpt: strings.Join(s.rcpts, ","),
		MsgID: msgID, Sha256: sum, Code: resp.Code, Enhanced: &resp.Enhanced, Text: &resp.Text, TLS: s.tls,
	})
	s.resetTx()
	return Result{
		Reply: &resp, Delay: s.latency(StageDataEnd),
		MsgID: msgID, Accepted: resp.Code >= 200 && resp.Code < 300,
	}
}

// Gone records that the client closed the connection or timed out.
func (s *Session) Gone() {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if !s.ended {
		s.end(EvClose, "")
	}
}

// Cut records that the simulator cut the connection on request.
func (s *Session) Cut(rule string) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if !s.ended {
		s.end(EvDrop, rule)
	}
}

// Ended reports whether the session is over.
func (s *Session) Ended() bool {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	return s.ended
}

func (s *Session) resetTx() {
	s.inTx, s.inData, s.mailFrom, s.rcpts = false, false, "", nil
}

func (s *Session) emit(e Event) {
	e.AtMs = s.r.nowMs()
	e.Conn = s.id
	e.SrcIP = s.src.String()
	e.DstIP = s.dst.String()
	e.Host = s.host
	e.Profile = s.profile.Name
	// A log write failure is fatal for the evidence; the transport sees it
	// through Sync on the next data_end. Append errors are not recoverable
	// here.
	_ = s.r.log.Append(e)
}

func (s *Session) reply(resp Response, stage string) Result {
	s.emit(Event{Ev: EvReply, Code: resp.Code, Enhanced: &resp.Enhanced, Text: &resp.Text})
	return Result{Reply: &resp, Delay: s.latency(stage)}
}

func (s *Session) end(ev, rule string) {
	s.ended = true
	s.emit(Event{Ev: ev, Rule: rule})
	s.st.open[s.src]--
}

// violate logs a violation of a limit rule and applies its on_violation.
// The violation is counted once adaptation_period_s has elapsed since the
// first connection of this source address to this profile (contract 3.1).
func (s *Session) violate(c *compiledRule, stage string) Result {
	elapsed := s.r.nowMs() - s.st.firstConn[s.src]
	counted := elapsed >= int64(s.profile.AdaptationPeriodS)*1000
	s.emit(Event{Ev: EvViolation, Rule: c.id, Counted: &counted})
	ov := c.onViolation
	if ov.Action == ActionCloseSilent {
		s.end(EvDrop, c.id)
		return Result{Close: true}
	}
	res := s.reply(render(*ov.Response, domainOf(s.mailFrom)), stage)
	if ov.Action == ActionRespondClose || stage == StageConnect {
		s.end(EvClose, "")
		res.Close = true
	}
	return res
}

// respondRule applies the first matching respond rule of a stage.
func (s *Session) respondRule(stage, rcpt string) (Result, bool) {
	c, ok := s.matchRespond(stage, rcpt)
	if c == nil || !ok {
		return Result{}, false
	}
	res := s.reply(render(c.respond.Response, domainOf(s.mailFrom)), stage)
	if c.respond.Close || stage == StageConnect {
		s.end(EvClose, "")
		res.Close = true
	}
	return res, true
}

func (s *Session) matchRespond(stage, rcpt string) (*compiledRule, bool) {
	for _, c := range s.profile.compiled {
		if c.kind != KindRespond || c.respond.Stage != stage {
			continue
		}
		if s.matches(c, stage, rcpt) {
			return c, true
		}
	}
	return nil, false
}

func (s *Session) matches(c *compiledRule, stage, rcpt string) bool {
	m := c.respond.Match
	if len(c.srcIPs) > 0 && !slices.ContainsFunc(c.srcIPs, func(p netip.Prefix) bool { return p.Contains(s.src) }) {
		return false
	}
	if m.MailFromDomain != "" && (stage == StageConnect || !strings.EqualFold(domainOf(s.mailFrom), m.MailFromDomain)) {
		return false
	}
	if m.RcptLocalPrefix == "" && m.RcptDomain == "" {
		return true
	}
	candidates := s.rcpts
	if stage == StageRcpt {
		candidates = []string{rcpt}
	} else if stage != StageDataEnd {
		return false
	}
	for _, r := range candidates {
		local, domain, _ := strings.Cut(r, "@")
		if m.RcptLocalPrefix != "" && !strings.HasPrefix(strings.ToLower(local), strings.ToLower(m.RcptLocalPrefix)) {
			continue
		}
		if m.RcptDomain != "" && !strings.EqualFold(domain, m.RcptDomain) {
			continue
		}
		return true
	}
	return false
}

func (s *Session) latency(stage string) time.Duration {
	var ms int
	for _, c := range s.profile.compiled {
		if c.kind == KindLatency && (c.latency.Stage == stage || c.latency.Stage == StageAny) {
			ms += c.latency.Ms
		}
	}
	return time.Duration(ms) * time.Millisecond
}

func (s *Session) rateKey(c *compiledRule, rcpt string) windowKey {
	switch c.rate.Scope {
	case ScopeProfile:
		return windowKey{c, ""}
	case ScopeRcpt:
		return windowKey{c, strings.ToLower(rcpt)}
	default:
		return windowKey{c, s.src.String()}
	}
}

// windowFull reports whether the sliding window of the rule already holds
// limit accepted commands within the last per_s seconds.
func (s *Session) windowFull(c *compiledRule, k windowKey, now int64) bool {
	w := s.st.windows[k]
	cutoff := now - int64(c.rate.PerS)*1000
	i := 0
	for i < len(w) && w[i] <= cutoff {
		i++
	}
	w = w[i:]
	s.st.windows[k] = w
	return len(w) >= c.rate.Limit
}

func (s *Session) record(c *compiledRule, k windowKey, now int64) {
	s.st.windows[k] = append(s.st.windows[k], now)
}

// parsePath extracts the address of "FROM:<addr> params" or "TO:<addr>".
func parsePath(arg, prefix string) (string, bool) {
	if len(arg) < len(prefix) || !strings.EqualFold(arg[:len(prefix)], prefix) {
		return "", false
	}
	rest := strings.TrimLeft(arg[len(prefix):], " ")
	if !strings.HasPrefix(rest, "<") {
		return "", false
	}
	end := strings.IndexByte(rest, '>')
	if end < 0 {
		return "", false
	}
	addr := rest[1:end]
	// Drop an obsolete source route (RFC 5321 §4.1.2, "@a,@b:user@x").
	if strings.HasPrefix(addr, "@") {
		if _, after, ok := strings.Cut(addr, ":"); ok {
			addr = after
		}
	}
	return addr, true
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return strings.ToLower(addr[i+1:])
	}
	return ""
}

// headerValue returns the unfolded value of the first header field called
// name in the message header section, or "".
func headerValue(msg []byte, name string) string {
	var cur string
	var found bool
	for _, raw := range bytes.Split(msg, []byte("\n")) {
		line := strings.TrimSuffix(string(raw), "\r")
		if line == "" {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			if found {
				cur += line
			}
			continue
		}
		if found {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), name) {
			cur, found = v, true
		}
	}
	return strings.TrimSpace(cur)
}
