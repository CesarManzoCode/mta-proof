package engine

import (
	"bytes"
	"net/mail"
	"net/textproto"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/CesarManzoCode/mta-proof/internal/clock"
	"github.com/CesarManzoCode/mta-proof/internal/store"
)

// simEngine drives the core the way the simulated executor does: one
// goroutine, virtual clock, every step committed before the next.
type simEngine struct {
	t    *testing.T
	path string
	clk  *clock.Virtual
	st   *store.Store
	core *Core
}

func newSim(t *testing.T) *simEngine {
	t.Helper()
	e := &simEngine{t: t, path: filepath.Join(t.TempDir(), "engine.db"), clk: clock.NewVirtual(time.UnixMilli(0))}
	e.open()
	t.Cleanup(func() { e.st.Close() })
	return e
}

func (e *simEngine) open() {
	e.t.Helper()
	st, err := store.Open(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	core, err := OpenCore(st, e.clk, 1)
	if err != nil {
		e.t.Fatal(err)
	}
	e.st, e.core = st, core
}

// reopen simulates a restart: the in-memory core is dropped and rebuilt
// from the database.
func (e *simEngine) reopen() {
	e.t.Helper()
	e.st.Close()
	e.open()
}

func (e *simEngine) commit() {
	e.t.Helper()
	if err := e.st.Apply(e.core.TakeWrites()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *simEngine) ingest(s *Submission, key string) IngestResult {
	e.t.Helper()
	if err := s.Validate(); err != nil {
		e.t.Fatal(err)
	}
	r := e.core.Ingest(s, key)
	e.commit()
	return r
}

func (e *simEngine) events(kind string) []store.Event {
	e.t.Helper()
	all, err := e.st.Events()
	if err != nil {
		e.t.Fatal(err)
	}
	var out []store.Event
	for _, ev := range all {
		if kind == "" || ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func sub(text string) *Submission {
	return &Submission{Tenant: "acme", From: "app@sender.test", To: "user@gmail-model.test",
		Subject: "Your receipt", Text: text, MsgType: TypeTransactional}
}

func TestSequentialIDsAcrossRestarts(t *testing.T) {
	e := newSim(t)
	for i, want := range []string{"m00000001", "m00000002"} {
		if r := e.ingest(sub("x"), ""); r.Status != 202 || r.MsgID != want {
			t.Fatalf("ingest %d: %+v, want 202 %s", i, r, want)
		}
	}
	e.reopen()
	if r := e.ingest(sub("x"), ""); r.MsgID != "m00000003" {
		t.Fatalf("after restart got %s, want m00000003", r.MsgID)
	}
	if n := len(e.events(KindAccepted)); n != 3 {
		t.Fatalf("%d accepted events, want 3", n)
	}
}

func TestIdempotentReplay(t *testing.T) {
	e := newSim(t)
	first := e.ingest(sub("hello"), "order-42")
	e.clk.Advance(time.Hour)
	again := e.ingest(sub("hello"), "order-42")
	if again.Status != 202 || !again.Replay || again.MsgID != first.MsgID {
		t.Fatalf("replay: %+v, want 202 replay of %s", again, first.MsgID)
	}
	if n := len(e.events(KindAccepted)); n != 1 {
		t.Fatalf("%d accepted events after a replay, want 1", n)
	}
	rep := e.events(KindIdempotentReplay)
	if len(rep) != 1 || rep[0].MsgID != first.MsgID || rep[0].AtMs != 3_600_000 ||
		!strings.Contains(rep[0].Detail, `"idempotency_key":"order-42"`) {
		t.Fatalf("idempotent_replay events: %+v", rep)
	}
	// The key survives a restart.
	e.reopen()
	if r := e.ingest(sub("hello"), "order-42"); !r.Replay || r.MsgID != first.MsgID {
		t.Fatalf("replay after restart: %+v", r)
	}
}

func TestIdempotencyConflict(t *testing.T) {
	e := newSim(t)
	first := e.ingest(sub("hello"), "order-42")
	other := sub("hello")
	other.To = "someone@yahoo-model.test"
	r := e.ingest(other, "order-42")
	if r.Status != 409 || r.MsgID != first.MsgID {
		t.Fatalf("conflict: %+v, want 409 naming %s", r, first.MsgID)
	}
	if n := len(e.events(KindAccepted)); n != 1 {
		t.Fatalf("%d accepted events after a conflict, want 1", n)
	}
	if c := e.events(KindIdempotencyConflict); len(c) != 1 || c[0].MsgID != first.MsgID {
		t.Fatalf("idempotency_conflict events: %+v", c)
	}
	// Payload identity is the parsed submission: field order does not matter,
	// any field does.
	tenant := sub("hello")
	tenant.Tenant = "other"
	if r := e.ingest(tenant, "order-42"); r.Status != 409 {
		t.Fatalf("same key, other tenant: %+v, want 409", r)
	}
}

func TestIdempotencyWindowExpires(t *testing.T) {
	e := newSim(t)
	first := e.ingest(sub("hello"), "order-42")
	e.clk.Advance(IdempotencyWindow - time.Millisecond)
	if r := e.ingest(sub("hello"), "order-42"); !r.Replay {
		t.Fatalf("1 ms before the window ends: %+v, want replay", r)
	}
	e.clk.Advance(time.Millisecond)
	fresh := e.ingest(sub("hello"), "order-42")
	if fresh.Status != 202 || fresh.Replay || fresh.MsgID == first.MsgID {
		t.Fatalf("after 24 h: %+v, want a new message", fresh)
	}
	// The key now belongs to the new message for another window, and a
	// different payload is accepted once the old key has expired.
	if r := e.ingest(sub("hello"), "order-42"); r.MsgID != fresh.MsgID || !r.Replay {
		t.Fatalf("replay of the renewed key: %+v", r)
	}
	e.clk.Advance(IdempotencyWindow)
	if r := e.ingest(sub("other text"), "order-42"); r.Status != 202 || r.Replay {
		t.Fatalf("other payload after expiry: %+v, want a new message", r)
	}
	if n := len(e.events(KindAccepted)); n != 3 {
		t.Fatalf("%d accepted events, want 3", n)
	}
}

func TestRecoveryRequeuesStartedAttempt(t *testing.T) {
	e := newSim(t)
	a := e.ingest(sub("a"), "").MsgID
	b := e.ingest(sub("b"), "").MsgID
	route := Route{Provider: "gmail-model", MXHost: "mx1.gmail-model.test", SrcIP: "127.0.2.1"}
	for _, id := range []string{a, b} {
		if n, err := e.core.StartAttempt(id, route); err != nil || n != 1 {
			t.Fatalf("start attempt %s: %d %v", id, n, err)
		}
	}
	e.commit()
	if err := e.core.FinishAttempt(b, Outcome{Kind: KindDelivered, Route: route, Code: 250, Text: "ok"}); err != nil {
		t.Fatal(err)
	}
	e.commit()
	// a has attempt_started and no result: crash and restart.
	e.clk.Advance(5 * time.Second)
	e.reopen()
	rec := e.events(KindRecovered)
	if len(rec) != 1 || rec[0].MsgID != a || rec[0].Attempt != 1 || rec[0].AtMs != 5000 {
		t.Fatalf("recovered events: %+v, want one for %s attempt 1 at 5000", rec, a)
	}
	if m := e.core.Message(a); m.State != store.StateQueued || m.NextAtMs != 5000 {
		t.Fatalf("recovered message state %s next %d", m.State, m.NextAtMs)
	}
	if m := e.core.Message(b); m.State != store.StateDelivered {
		t.Fatalf("delivered message came back as %s", m.State)
	}
	due := e.core.Due(0)
	if len(due) != 1 || due[0].ID != a {
		t.Fatalf("due after recovery: %v", due)
	}
	if n, err := e.core.StartAttempt(a, route); err != nil || n != 2 {
		t.Fatalf("next attempt: %d %v, want 2", n, err)
	}
	e.commit()
	// Restarting with attempt 2 inflight recovers it; a further restart,
	// with nothing inflight, recovers nothing.
	e.reopen()
	if n := len(e.events(KindRecovered)); n != 2 {
		t.Fatalf("%d recovered events, want 2", n)
	}
	e.reopen()
	if n := len(e.events(KindRecovered)); n != 2 {
		t.Fatalf("restart without inflight attempts added recovered events: %d", n)
	}
}

func TestAttemptTransitions(t *testing.T) {
	e := newSim(t)
	id := e.ingest(sub("a"), "").MsgID
	if err := e.core.FinishAttempt(id, Outcome{Kind: KindDelivered}); err != ErrBadState {
		t.Fatalf("finish without start: %v", err)
	}
	e.core.StartAttempt(id, Route{})
	if _, err := e.core.StartAttempt(id, Route{}); err != ErrBadState {
		t.Fatalf("second start while inflight: %v", err)
	}
	e.clk.Advance(time.Second)
	if err := e.core.FinishAttempt(id, Outcome{Kind: KindDeferred, Code: 451, Enhanced: "4.3.0", NextAtMs: 60_000}); err != nil {
		t.Fatal(err)
	}
	e.commit()
	if len(e.core.Due(0)) != 0 {
		t.Fatal("deferred message is due before its time")
	}
	e.clk.Advance(time.Minute)
	if d := e.core.Due(0); len(d) != 1 {
		t.Fatal("deferred message is not due after its time")
	}
	if _, err := e.core.StartAttempt("m99999999", Route{}); err != ErrNotFound {
		t.Fatalf("unknown message: %v", err)
	}
	def := e.events(KindDeferred)
	if len(def) != 1 || def[0].Code != 451 || def[0].Enhanced != "4.3.0" || def[0].Attempt != 1 {
		t.Fatalf("deferred event: %+v", def)
	}
}

// msgIDRe is msg-id of RFC 5322 §3.6.4 with dot-atom-text on both sides.
var msgIDRe = regexp.MustCompile(`^<[A-Za-z0-9!#$%&'*+/=?^_` + "`" + `{|}~-]+(\.[A-Za-z0-9!#$%&'*+/=?^_` + "`" + `{|}~-]+)*@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*>$`)

func TestGeneratedMessageHeaders(t *testing.T) {
	e := newSim(t)
	e.clk.Set(time.Date(2026, 10, 22, 13, 5, 9, 0, time.UTC))
	long := strings.Repeat("ñandú ", 400) + "\n.line starting with a dot\r\nlast"
	id := e.ingest(sub(long), "").MsgID
	raw := e.core.Message(id).Raw
	for i, line := range bytes.Split(raw, []byte("\r\n")) {
		if len(line) > 998 || bytes.ContainsAny(line, "\r\n") {
			t.Fatalf("line %d is %d bytes or has a bare CR/LF", i, len(line))
		}
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	// §3.6: Date and From exactly once; To, Subject and Message-ID at most
	// once (here exactly once).
	for _, h := range []string{"Date", "From", "To", "Subject", "Message-ID", "X-Mtaproof-Id"} {
		if n := len(msg.Header[textproto.CanonicalMIMEHeaderKey(h)]); n != 1 {
			t.Fatalf("%s appears %d times", h, n)
		}
	}
	date, err := msg.Header.Date()
	if err != nil || !date.Equal(e.clk.Now()) {
		t.Fatalf("Date %q parses to %v (%v), want the ingest time", msg.Header.Get("Date"), date, err)
	}
	if got := msg.Header.Get("Date"); got != "Thu, 22 Oct 2026 13:05:09 +0000" {
		t.Fatalf("Date %q", got)
	}
	for _, h := range []string{"From", "To"} {
		if _, err := mail.ParseAddress(msg.Header.Get(h)); err != nil {
			t.Fatalf("%s: %v", h, err)
		}
	}
	mid := msg.Header.Get("Message-ID")
	if !msgIDRe.MatchString(mid) || mid != MessageID(id, "app@sender.test") || !strings.Contains(mid, id) {
		t.Fatalf("Message-ID %q is not a msg-id derived from %s", mid, id)
	}
	if got := msg.Header.Get("X-Mtaproof-Id"); got != id {
		t.Fatalf("X-Mtaproof-Id %q, want %s", got, id)
	}
}

func TestValidate(t *testing.T) {
	bad := []func(*Submission){
		func(s *Submission) { s.To = "a@x.test, b@x.test" },
		func(s *Submission) { s.To = "Name <a@x.test>" },
		func(s *Submission) { s.From = "" },
		func(s *Submission) { s.Subject = "two\r\nBcc: x@y.test" },
		func(s *Submission) { s.Subject = "" },
		func(s *Submission) { s.MsgType = "bulk" },
		func(s *Submission) { s.Tenant = "a b" },
		func(s *Submission) { s.Text = "\xff" },
	}
	for i, f := range bad {
		s := sub("x")
		f(s)
		if s.Validate() == nil {
			t.Errorf("case %d accepted: %+v", i, s)
		}
	}
	if err := sub("x").Validate(); err != nil {
		t.Fatal(err)
	}
}
