package sim

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

var (
	ipKnown   = netip.MustParseAddr("127.0.2.1")
	ipOther   = netip.MustParseAddr("127.0.2.2")
	ipThird   = netip.MustParseAddr("127.0.2.3")
	ipUnknown = netip.MustParseAddr("127.0.2.100")
	ipMX      = netip.MustParseAddr("127.0.1.1")
)

// testProfile builds a profile named p serving mx.p.test with the rules.
func testProfile(t *testing.T, adaptation int, rules string) *Profile {
	t.Helper()
	js := `{"name": "p", "sources": ["R-YAHOO", "R-GOOGLE-ERR", "R-5321"], "limitations": "test",
		"adaptation_period_s": ` + itoa(adaptation) + `, "hosts": ["mx.p.test"], "rules": [` + rules + `]}`
	p, err := ParseProfile([]byte(js), nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func newTestReceiver(t *testing.T, profiles ...*Profile) (*Receiver, *fakeClock, string) {
	t.Helper()
	dir := t.TempDir()
	clock := &fakeClock{t: time.UnixMilli(1_000_000)}
	m := map[string]*Profile{}
	for _, p := range profiles {
		m[p.Name] = p
	}
	r, err := NewReceiver(Config{Dir: dir, Profiles: m, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, clock, dir
}

func readEvents(t *testing.T, dir string) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "receiver.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e map[string]any
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad log line %q: %v", sc.Text(), err)
		}
		out = append(out, e)
	}
	return out
}

func eventsOf(evs []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, e := range evs {
		if e["ev"] == kind {
			out = append(out, e)
		}
	}
	return out
}

func open(t *testing.T, r *Receiver, src netip.Addr) (*Session, Result) {
	t.Helper()
	s, res, err := r.Open("mx.p.test", src, ipMX)
	if err != nil {
		t.Fatal(err)
	}
	return s, res
}

func code(res Result) int {
	if res.Reply == nil {
		return 0
	}
	return res.Reply.Code
}

// mustCode feeds a line and checks the reply code (0: no reply).
func mustCode(t *testing.T, s *Session, line string, want int) Result {
	t.Helper()
	res := s.Line(line)
	if code(res) != want {
		t.Fatalf("%q: got %v, want %d", line, res.Reply, want)
	}
	return res
}

func message(id string) []byte {
	return []byte("From: a@sender.test\r\nX-Mtaproof-Id: " + id + "\r\nSubject: t\r\n\r\nbody\r\n")
}

// deliver runs one complete transaction and returns the reply to DATA.
func deliver(t *testing.T, s *Session, rcpt, id string) Result {
	t.Helper()
	mustCode(t, s, "MAIL FROM:<a@sender.test>", 250)
	mustCode(t, s, "RCPT TO:<"+rcpt+">", 250)
	mustCode(t, s, "DATA", 354)
	return s.Data(message(id))
}

func TestConversationLog(t *testing.T) {
	r, _, dir := newTestReceiver(t, testProfile(t, 0, ""))
	s, res := open(t, r, ipKnown)
	if code(res) != 220 {
		t.Fatalf("greeting %v", res.Reply)
	}
	mustCode(t, s, "MAIL FROM:<a@sender.test>", 503) // RFC 5321 §4.1.4: EHLO first
	res = mustCode(t, s, "EHLO client.test", 250)
	if !strings.HasPrefix(string(res.Wire()), "250-mx.p.test greets client.test\r\n") {
		t.Fatalf("EHLO wire %q", res.Wire())
	}
	mustCode(t, s, "MAIL FROM:<>", 250)
	mustCode(t, s, "RCPT TO:<u@p.test>", 250)
	mustCode(t, s, "DATA", 354)
	msg := message("m-1")
	res = s.Data(msg)
	if code(res) != 250 || !res.Accepted || res.MsgID != "m-1" {
		t.Fatalf("data end %+v", res)
	}
	res = mustCode(t, s, "QUIT", 221)
	if !res.Close {
		t.Fatal("QUIT must close")
	}

	evs := readEvents(t, dir)
	var kinds []string
	for i, e := range evs {
		kinds = append(kinds, e["ev"].(string))
		if e["seq"].(float64) != float64(i+1) {
			t.Fatalf("seq of line %d is %v", i, e["seq"])
		}
		if e["profile"] != "p" || e["host"] != "mx.p.test" || e["src_ip"] != "127.0.2.1" || e["dst_ip"] != "127.0.1.1" {
			t.Fatalf("common fields %v", e)
		}
	}
	want := "connect reply reply ehlo reply mail reply rcpt reply reply data_end reply close"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("events\n got %s\nwant %s", got, want)
	}
	de := eventsOf(evs, EvDataEnd)[0]
	sum := sha256.Sum256(msg)
	if de["msg_id"] != "m-1" || de["sha256"] != hex.EncodeToString(sum[:]) || de["code"] != 250.0 || de["enhanced"] != "2.0.0" {
		t.Fatalf("data_end %v", de)
	}
	if mf, ok := de["mail_from"]; !ok || mf != "" {
		t.Fatalf("null reverse-path must be logged as empty mail_from, got %v", de)
	}
	stored, err := os.ReadFile(filepath.Join(dir, "messages", hex.EncodeToString(sum[:])+".eml"))
	if err != nil || string(stored) != string(msg) {
		t.Fatalf("stored message %q, %v", stored, err)
	}
}

func TestMaxMsgsPerConnSilentCloseAndAdaptation(t *testing.T) {
	p := testProfile(t, 300, `{"id": "y.max", "kind": "max_msgs_per_conn", "params": {"limit": 2},
		"on_violation": {"action": "close_silent"}, "source": "R-YAHOO", "assumption": true}`)
	r, clock, dir := newTestReceiver(t, p)

	run := func() {
		s, _ := open(t, r, ipKnown)
		mustCode(t, s, "EHLO c.test", 250)
		for i := 0; i < 2; i++ {
			if code(deliver(t, s, "u@p.test", "m")) != 250 {
				t.Fatal("message under the limit refused")
			}
		}
		res := s.Line("MAIL FROM:<a@sender.test>")
		if res.Reply != nil || !res.Close {
			t.Fatalf("third MAIL: want silent close, got %+v", res)
		}
	}
	run() // inside the adaptation period
	clock.advance(301 * time.Second)
	run() // after it

	evs := readEvents(t, dir)
	vs := eventsOf(evs, EvViolation)
	if len(vs) != 2 || vs[0]["counted"] != false || vs[1]["counted"] != true || vs[0]["rule"] != "y.max" {
		t.Fatalf("violations %v", vs)
	}
	drops := eventsOf(evs, EvDrop)
	if len(drops) != 2 || drops[0]["rule"] != "y.max" {
		t.Fatalf("drops %v", drops)
	}
	if n := len(eventsOf(evs, EvDataEnd)); n != 4 {
		t.Fatalf("data_end count %d", n)
	}
}

const gmailRate = `{"code": 421, "enhanced": "4.7.28", "text": "Gmail has detected an unusual rate of email originating from your IP address"}`

func TestRateSrcIP(t *testing.T) {
	p := testProfile(t, 0, `{"id": "g.rate", "kind": "rate", "params": {"scope": "src_ip", "limit": 2, "per_s": 60},
		"on_violation": {"action": "respond_close", "response": `+gmailRate+`}, "source": "R-GOOGLE-ERR", "assumption": true}`)
	r, clock, dir := newTestReceiver(t, p)
	a, _ := open(t, r, ipKnown)
	mustCode(t, a, "EHLO c.test", 250)
	deliver(t, a, "u@p.test", "1")
	deliver(t, a, "u@p.test", "2")
	res := mustCode(t, a, "MAIL FROM:<a@sender.test>", 421)
	if !res.Close || res.Reply.Text != "Gmail has detected an unusual rate of email originating from your IP address" {
		t.Fatalf("violation reply %+v", res)
	}
	// Another source address has its own window.
	b, _ := open(t, r, ipOther)
	mustCode(t, b, "EHLO c.test", 250)
	deliver(t, b, "u@p.test", "3")
	// The window slides.
	clock.advance(61 * time.Second)
	a2, _ := open(t, r, ipKnown)
	mustCode(t, a2, "EHLO c.test", 250)
	deliver(t, a2, "u@p.test", "4")

	vs := eventsOf(readEvents(t, dir), EvViolation)
	if len(vs) != 1 || vs[0]["counted"] != true || vs[0]["src_ip"] != "127.0.2.1" {
		t.Fatalf("violations %v", vs)
	}
}

func TestRateProfileScope(t *testing.T) {
	p := testProfile(t, 0, `{"id": "g.rate_all", "kind": "rate", "params": {"scope": "profile", "limit": 2, "per_s": 60},
		"on_violation": {"action": "respond_close", "response": `+gmailRate+`}, "source": "R-GOOGLE-ERR", "assumption": true}`)
	r, _, _ := newTestReceiver(t, p)
	for i, ip := range []netip.Addr{ipKnown, ipOther} {
		s, _ := open(t, r, ip)
		mustCode(t, s, "EHLO c.test", 250)
		deliver(t, s, "u@p.test", itoa(i))
	}
	s, _ := open(t, r, ipThird)
	mustCode(t, s, "EHLO c.test", 250)
	mustCode(t, s, "MAIL FROM:<a@sender.test>", 421)
}

func TestRateRcptKeepsConnection(t *testing.T) {
	p := testProfile(t, 0, `{"id": "g.rcpt", "kind": "rate", "params": {"scope": "rcpt", "limit": 1, "per_s": 60},
		"on_violation": {"action": "respond", "response": {"code": 450, "enhanced": "4.2.1", "text": "The user you are trying to contact is receiving email too quickly."}},
		"source": "R-GOOGLE-ERR", "assumption": true}`)
	r, _, dir := newTestReceiver(t, p)
	s, _ := open(t, r, ipKnown)
	mustCode(t, s, "EHLO c.test", 250)
	deliver(t, s, "u@p.test", "1")
	mustCode(t, s, "MAIL FROM:<a@sender.test>", 250)
	res := mustCode(t, s, "RCPT TO:<U@p.test>", 450)
	if res.Close {
		t.Fatal("450 must keep the connection")
	}
	mustCode(t, s, "RCPT TO:<v@p.test>", 250)
	mustCode(t, s, "DATA", 354)
	if code(s.Data(message("2"))) != 250 {
		t.Fatal("other recipient refused")
	}
	if vs := eventsOf(readEvents(t, dir), EvViolation); len(vs) != 1 || vs[0]["rule"] != "g.rcpt" {
		t.Fatalf("violations %v", vs)
	}
}

func TestConcurrency(t *testing.T) {
	p := testProfile(t, 0, `{"id": "g.conc", "kind": "concurrency", "params": {"limit": 1},
		"on_violation": {"action": "respond_close", "response": {"code": 421, "enhanced": "4.7.0", "text": "Try again later, closing connection."}},
		"source": "R-GOOGLE-ERR", "assumption": true}`)
	r, _, dir := newTestReceiver(t, p)
	s1, _ := open(t, r, ipKnown)
	_, res := open(t, r, ipKnown)
	if code(res) != 421 || !res.Close {
		t.Fatalf("second connection %+v", res)
	}
	// Another address is not affected.
	if _, res := open(t, r, ipOther); code(res) != 220 {
		t.Fatalf("other address %+v", res)
	}
	mustCode(t, s1, "QUIT", 221)
	if _, res := open(t, r, ipKnown); code(res) != 220 {
		t.Fatalf("after close %+v", res)
	}
	if vs := eventsOf(readEvents(t, dir), EvViolation); len(vs) != 1 {
		t.Fatalf("violations %v", vs)
	}
}

func TestUnknownIP(t *testing.T) {
	p := testProfile(t, 0, `{"id": "g.unknown", "kind": "unknown_ip",
		"params": {"known_ips": ["127.0.2.0/28"], "rate": {"limit": 1, "per_s": 60}, "max_msgs_per_conn": 5},
		"on_violation": {"action": "respond_close", "response": `+gmailRate+`}, "source": "R-GOOGLE-ERR", "assumption": true}`)
	r, _, dir := newTestReceiver(t, p)
	k, _ := open(t, r, ipKnown)
	mustCode(t, k, "EHLO c.test", 250)
	for i := 0; i < 3; i++ {
		if code(deliver(t, k, "u@p.test", itoa(i))) != 250 {
			t.Fatal("known address throttled")
		}
	}
	u, _ := open(t, r, ipUnknown)
	mustCode(t, u, "EHLO c.test", 250)
	deliver(t, u, "u@p.test", "u1")
	mustCode(t, u, "MAIL FROM:<a@sender.test>", 421)
	vs := eventsOf(readEvents(t, dir), EvViolation)
	if len(vs) != 1 || vs[0]["rule"] != "g.unknown" || vs[0]["src_ip"] != "127.0.2.100" {
		t.Fatalf("violations %v", vs)
	}
}

func TestRespond(t *testing.T) {
	p := testProfile(t, 0, `
		{"id": "r.nouser", "kind": "respond", "params": {"stage": "rcpt", "match": {"rcpt_local_prefix": "nouser"},
		 "response": {"code": 550, "enhanced": "5.1.1", "text": "The email account that you tried to reach does not exist."}},
		 "source": "R-GOOGLE-ERR", "assumption": false},
		{"id": "r.dmarc", "kind": "respond", "params": {"stage": "mail", "match": {"mail_from_domain": "spoof.test"},
		 "response": {"code": 550, "enhanced": "5.7.26", "text": "Unauthenticated email from domain-name is not accepted due to domain's DMARC policy."}},
		 "source": "R-GOOGLE-ERR", "assumption": false},
		{"id": "r.defer", "kind": "respond", "params": {"stage": "data_end", "match": {"rcpt_domain": "defer.test"},
		 "response": {"code": 451, "enhanced": "4.3.0", "text": "Requested action aborted: local error in processing"}},
		 "source": "R-5321", "assumption": false},
		{"id": "r.block", "kind": "respond", "params": {"stage": "connect", "match": {"src_ips": ["127.0.2.3"]},
		 "response": {"code": 421, "enhanced": "4.7.0", "text": "Try again later, closing connection."}, "close": true},
		 "source": "R-GOOGLE-ERR", "assumption": false}`)
	r, _, dir := newTestReceiver(t, p)
	s, _ := open(t, r, ipKnown)
	mustCode(t, s, "EHLO c.test", 250)
	mustCode(t, s, "MAIL FROM:<a@sender.test>", 250)
	res := mustCode(t, s, "RCPT TO:<nouser1@p.test>", 550)
	if res.Close {
		t.Fatal("550 at RCPT must not close")
	}
	mustCode(t, s, "RCPT TO:<x@defer.test>", 250)
	mustCode(t, s, "DATA", 354)
	res = s.Data(message("m-9"))
	if code(res) != 451 || res.Accepted {
		t.Fatalf("data_end %+v", res)
	}
	res = mustCode(t, s, "MAIL FROM:<x@spoof.test>", 550)
	if want := "Unauthenticated email from spoof.test is not accepted due to domain's DMARC policy."; res.Reply.Text != want {
		t.Fatalf("placeholder not substituted: %q", res.Reply.Text)
	}
	if _, res := open(t, r, ipThird); code(res) != 421 || !res.Close {
		t.Fatalf("connect respond %+v", res)
	}
	evs := readEvents(t, dir)
	de := eventsOf(evs, EvDataEnd)
	if len(de) != 1 || de[0]["code"] != 451.0 || de[0]["msg_id"] != "m-9" {
		t.Fatalf("data_end %v", de)
	}
	if len(eventsOf(evs, EvViolation)) != 0 {
		t.Fatal("respond rules are not violations")
	}
}

func TestDropMidData(t *testing.T) {
	p := testProfile(t, 0, `{"id": "d.drop", "kind": "drop_mid_data", "params": {"every_n": 2, "after_bytes": 10}, "assumption": true}`)
	r, _, dir := newTestReceiver(t, p)
	s, _ := open(t, r, ipKnown)
	mustCode(t, s, "EHLO c.test", 250)
	if code(deliver(t, s, "u@p.test", "1")) != 250 {
		t.Fatal("first message must pass")
	}
	mustCode(t, s, "MAIL FROM:<a@sender.test>", 250)
	mustCode(t, s, "RCPT TO:<u@p.test>", 250)
	res := mustCode(t, s, "DATA", 354)
	if !res.DropMidData || res.DropAfter != 10 {
		t.Fatalf("second DATA must drop: %+v", res)
	}
	if res := s.DropData(); res.Reply != nil || !res.Close {
		t.Fatalf("drop %+v", res)
	}
	evs := readEvents(t, dir)
	if len(eventsOf(evs, EvDataEnd)) != 1 {
		t.Fatal("a dropped DATA has no data_end")
	}
	if d := eventsOf(evs, EvDrop); len(d) != 1 || d[0]["rule"] != "d.drop" {
		t.Fatalf("drop events %v", d)
	}
}

func TestLatency(t *testing.T) {
	p := testProfile(t, 0, `
		{"id": "l.end", "kind": "latency", "params": {"stage": "data_end", "ms": 150}, "assumption": true},
		{"id": "l.any", "kind": "latency", "params": {"stage": "any", "ms": 5}, "assumption": true}`)
	r, _, _ := newTestReceiver(t, p)
	s, res := open(t, r, ipKnown)
	if res.Delay != 5*time.Millisecond {
		t.Fatalf("greeting delay %v", res.Delay)
	}
	mustCode(t, s, "EHLO c.test", 250)
	if res := deliver(t, s, "u@p.test", "1"); res.Delay != 155*time.Millisecond {
		t.Fatalf("data_end delay %v", res.Delay)
	}
}

func TestAuthAcceptedButNotEnforced(t *testing.T) {
	p := testProfile(t, 0, `{"id": "a.auth", "kind": "auth", "params": {"require": ["spf", "dkim"],
		"fail_response": {"code": 550, "enhanced": "5.7.26", "text": "This email has been blocked because the sender is unauthenticated."}},
		"source": "R-GOOGLE-ERR", "assumption": false}`)
	r, _, _ := newTestReceiver(t, p)
	s, _ := open(t, r, ipKnown)
	mustCode(t, s, "EHLO c.test", 250)
	if code(deliver(t, s, "u@p.test", "1")) != 250 {
		t.Fatal("auth is enforced only from k2-04")
	}
}

func TestSwitchProfileAffectsNewConnections(t *testing.T) {
	base := testProfile(t, 0, "")
	defer1 := testProfile(t, 0, `{"id": "x", "kind": "respond", "params": {"stage": "data_end", "match": {},
		"response": {"code": 451, "enhanced": "4.3.0", "text": "Requested action aborted: local error in processing"}},
		"source": "R-5321", "assumption": false}`)
	defer1.Name, defer1.Hosts = "defer", nil
	r, _, _ := newTestReceiver(t, base, defer1)
	old, _ := open(t, r, ipKnown)
	mustCode(t, old, "EHLO c.test", 250)
	if err := r.SwitchProfile("mx.p.test", "defer"); err != nil {
		t.Fatal(err)
	}
	if code(deliver(t, old, "u@p.test", "1")) != 250 {
		t.Fatal("open session must keep its profile")
	}
	s, _ := open(t, r, ipKnown)
	mustCode(t, s, "EHLO c.test", 250)
	if code(deliver(t, s, "u@p.test", "2")) != 451 {
		t.Fatal("new session must use the new profile")
	}
}

func TestProtocolEdges(t *testing.T) {
	r, _, _ := newTestReceiver(t, testProfile(t, 0, ""))
	s, _ := open(t, r, ipKnown)
	mustCode(t, s, "FOO", 500)
	mustCode(t, s, "EHLO", 501)
	mustCode(t, s, "HELO c.test", 250)
	mustCode(t, s, "RCPT TO:<u@p.test>", 503)
	mustCode(t, s, "MAIL FROM:a@b", 501)
	mustCode(t, s, "MAIL FROM: <a@sender.test> BODY=8BITMIME", 250)
	mustCode(t, s, "MAIL FROM:<a@sender.test>", 503)
	mustCode(t, s, "DATA", 503)
	mustCode(t, s, "RSET", 250)
	mustCode(t, s, "NOOP", 250)
	mustCode(t, s, "STARTTLS", 502)
}

func TestHeaderValue(t *testing.T) {
	msg := []byte("Subject: x\r\nX-MTAPROOF-ID:\r\n  m-42\r\nOther: y\r\n\r\nX-Mtaproof-Id: body\r\n")
	if got := headerValue(msg, "X-Mtaproof-Id"); got != "m-42" {
		t.Fatalf("got %q", got)
	}
	if got := headerValue([]byte("A: b\r\n\r\nX-Mtaproof-Id: no\r\n"), "X-Mtaproof-Id"); got != "" {
		t.Fatalf("body header picked: %q", got)
	}
}

// TestSessionSTARTTLS follows R-3207: STARTTLS is offered only when the
// receiver is configured for it and not again over TLS, a completed
// handshake resets the session to the greeting state and is logged with
// its version and suite, and a message received over TLS carries them on
// its data_end.
func TestSessionSTARTTLS(t *testing.T) {
	p := testProfile(t, 0, "")
	plain, _, _ := newTestReceiver(t, p)
	s, _, _ := plain.Open("mx.p.test", ipKnown, ipMX)
	if res := s.Line("EHLO c.test"); strings.Contains(string(res.Wire()), "STARTTLS") {
		t.Fatalf("STARTTLS offered without TLS: %q", res.Wire())
	}
	if res := s.Line("STARTTLS"); res.Reply.Code != 502 || res.StartTLS {
		t.Fatalf("STARTTLS without TLS: %+v", res)
	}

	dir := t.TempDir()
	r, err := NewReceiver(Config{Dir: dir, Profiles: map[string]*Profile{"p": p}, Clock: &fakeClock{t: time.UnixMilli(1)}, StartTLS: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	s, _, _ = r.Open("mx.p.test", ipKnown, ipMX)
	if res := s.Line("STARTTLS"); res.Reply.Code != 503 {
		t.Fatalf("STARTTLS before EHLO: %+v", res.Reply)
	}
	if res := s.Line("EHLO c.test"); !strings.Contains(string(res.Wire()), "250-STARTTLS") {
		t.Fatalf("STARTTLS not offered: %q", res.Wire())
	}
	if res := s.Line("STARTTLS now"); res.Reply.Code != 501 {
		t.Fatalf("STARTTLS with a parameter: %+v", res.Reply)
	}
	if res := s.Line("STARTTLS"); res.Reply.Code != 220 || !res.StartTLS {
		t.Fatalf("STARTTLS: %+v", res)
	}
	s.TLSStarted("TLS 1.3", "TLS_AES_128_GCM_SHA256")
	if res := s.Line("MAIL FROM:<a@b.test>"); res.Reply.Code != 503 {
		t.Fatalf("MAIL right after the handshake: %+v", res.Reply)
	}
	if res := s.Line("EHLO c.test"); strings.Contains(string(res.Wire()), "STARTTLS") {
		t.Fatalf("STARTTLS offered over TLS: %q", res.Wire())
	}
	s.Line("MAIL FROM:<a@b.test>")
	s.Line("RCPT TO:<u@p.test>")
	s.Line("DATA")
	if res := s.Data([]byte("X-Mtaproof-Id: m1\r\n\r\nhi\r\n")); res.Reply.Code != 250 {
		t.Fatalf("data: %+v", res.Reply)
	}
	s2, _, _ := r.Open("mx.p.test", ipKnown, ipMX)
	s2.Line("EHLO c.test")
	s2.Line("STARTTLS")
	s2.TLSFailed(errors.New("remote error: tls: bad certificate"))
	if !s2.Ended() {
		t.Fatal("session alive after a failed handshake")
	}
	evs := readEvents(t, dir)
	st := eventsOf(evs, EvStartTLS)
	if len(st) != 2 || st[0]["tls"].(map[string]any)["version"] != "TLS 1.3" ||
		st[1]["tls"].(map[string]any)["error"] != "remote error: tls: bad certificate" {
		t.Fatalf("starttls lines %v", st)
	}
	de := eventsOf(evs, EvDataEnd)
	if len(de) != 1 || de[0]["tls"].(map[string]any)["cipher"] != "TLS_AES_128_GCM_SHA256" {
		t.Fatalf("data_end %v", de)
	}
}
