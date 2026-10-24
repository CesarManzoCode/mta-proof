package engine

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/CesarManzoCode/mta-proof/internal/clock"
	"github.com/CesarManzoCode/mta-proof/internal/store"
)

func TestOrderMX(t *testing.T) {
	in := []MXHost{mx(20, "c.test"), mx(10, "a.test"), mx(20, "b.test"), mx(30, "d.test"), mx(20, "e.test")}
	first := map[string]int{}
	for seed := uint64(1); seed <= 200; seed++ {
		out := orderMX(in, clock.Rand(seed, "engine.mx").Shuffle)
		names := mxNames(out)
		if names[0] != "a.test" || names[4] != "d.test" {
			t.Fatalf("seed %d: preference order broken: %v", seed, names)
		}
		mid := slices.Clone(names[1:4])
		slices.Sort(mid)
		if !slices.Equal(mid, []string{"b.test", "c.test", "e.test"}) {
			t.Fatalf("seed %d: equal-preference group changed: %v", seed, names)
		}
		first[names[1]]++
		// Same seed, same order, whatever the input order.
		rev := slices.Clone(in)
		slices.Reverse(rev)
		if again := mxNames(orderMX(rev, clock.Rand(seed, "engine.mx").Shuffle)); !slices.Equal(again, names) {
			t.Fatalf("seed %d: order depends on the DNS answer order: %v vs %v", seed, again, names)
		}
	}
	// R-5321 §5.1: equal preferences MUST be randomised. Each of the three
	// hosts leads the group in roughly a third of 200 seeds.
	for _, h := range []string{"b.test", "c.test", "e.test"} {
		if first[h] < 40 {
			t.Fatalf("equal-preference host %s first in %d of 200 orders: %v", h, first[h], first)
		}
	}
}

func TestMXFallbackInPreferenceOrder(t *testing.T) {
	e := newDeliveryEnv(t, testConfig())
	down := &fakeHost{name: "mx1.multi.test", down: true}
	up := &fakeHost{name: "mx2.multi.test"}
	e.net.add(down, "127.0.1.10")
	e.net.add(up, "127.0.1.11")
	e.res.answers["multi.test"] = Resolution{Status: ResolvedOK,
		MX: []MXHost{mx(20, "mx2.multi.test", "127.0.1.11"), mx(10, "mx1.multi.test", "127.0.1.10")}}
	id := e.ingest("acme", "app@sender.test", "user@multi.test")
	e.run(time.Minute)
	e.allState(store.StateDelivered, id)
	if want := []string{"mx1.multi.test 127.0.1.10", "mx2.multi.test 127.0.1.11"}; !slices.Equal(e.net.dials, want) {
		t.Fatalf("dials %v, want %v", e.net.dials, want)
	}
	routed := e.events(id, KindRouted)
	if len(routed) != 1 || routed[0].MXHost != "mx2.multi.test" {
		t.Fatalf("routed: %+v", routed)
	}
	var d struct {
		MX    []string    `json:"mx"`
		Tried []TryResult `json:"tried"`
	}
	json.Unmarshal([]byte(routed[0].Detail), &d)
	if !slices.Equal(d.MX, []string{"mx1.multi.test", "mx2.multi.test"}) || len(d.Tried) != 1 || d.Tried[0].Host != "mx1.multi.test" {
		t.Fatalf("routed detail: %s", routed[0].Detail)
	}
	if got := e.kinds(id); got != "accepted,attempt_started,routed,delivered" {
		t.Fatalf("events %s", got)
	}

	// With every MX down the attempt is a counted deferral.
	up.down = true
	now := clock.Ms(e.clk.Now())
	id2 := e.ingest("acme", "app@sender.test", "user@multi.test")
	e.run(0)
	def := e.events(id2, KindDeferred)
	if len(def) != 1 || def[0].RuleID != "rfc5321.mx_unreachable" || def[0].Subtype != "Connection" {
		t.Fatalf("deferred: %+v", def)
	}
	if m := e.core.Message(id2); m.Retries != 1 || m.NextAtMs != now+60_000 {
		t.Fatalf("after unreachable: retries %d next %d", m.Retries, m.NextAtMs)
	}
}

func TestImplicitMX(t *testing.T) {
	e := newDeliveryEnv(t, testConfig())
	h := &fakeHost{name: "implicit.test"}
	e.net.add(h, "127.0.1.20")
	e.res.answers["implicit.test"] = Resolution{Status: ResolvedOK, Implicit: true,
		MX: []MXHost{mx(0, "implicit.test", "127.0.1.20")}}
	id := e.ingest("acme", "app@sender.test", "user@implicit.test")
	e.run(time.Minute)
	e.allState(store.StateDelivered, id)
	if len(h.got) != 1 || e.events(id, KindDelivered)[0].MXHost != "implicit.test" {
		t.Fatalf("implicit MX delivery: %+v", h.got)
	}
}

func TestNullMXBouncesWithoutAttempts(t *testing.T) {
	e := newDeliveryEnv(t, testConfig())
	e.res.answers["nullmx.test"] = Resolution{Status: ResolvedNullMX}
	a := e.ingest("acme", "app@sender.test", "a@nullmx.test")
	e.run(time.Hour)
	e.clk.Advance(2 * time.Minute) // a later message uses the cached route (route_ttl_s 300)
	b := e.ingest("acme", "app@sender.test", "b@nullmx.test")
	e.run(48 * time.Hour)
	e.allState(store.StateBounced, a, b)
	if len(e.net.dials) != 0 {
		t.Fatalf("null MX caused connections: %v", e.net.dials)
	}
	for _, id := range []string{a, b} {
		if got := e.kinds(id); got != "accepted,routed,classified,bounced" {
			t.Fatalf("%s events %s", id, got)
		}
		bo := e.events(id, KindBounced)[0]
		if bo.Code != 556 || bo.Enhanced != "5.1.10" || bo.Class != "permanent" || bo.Subtype != "NullMX" ||
			bo.RuleID != "rfc7505.null_mx" || bo.Attempt != 0 {
			t.Fatalf("bounced: %+v", bo)
		}
	}
	if e.res.lookups["nullmx.test"] != 1 {
		t.Fatalf("lookups %d", e.res.lookups["nullmx.test"])
	}
	if n := len(e.events("", KindDSNEnqueued)); n != 0 {
		t.Fatalf("%d DSNs for bounces", n)
	}
}

func TestNXDomainAndTemporaryDNS(t *testing.T) {
	e := newDeliveryEnv(t, testConfig())
	nx := e.ingest("acme", "app@sender.test", "a@nowhere.test")
	e.res.answers["flaky.test"] = Resolution{Status: ResolvedTemporary, Err: "MX flaky.test: i/o timeout"}
	tmp := e.ingest("acme", "app@sender.test", "a@flaky.test")
	e.run(0)
	e.allState(store.StateBounced, nx)
	if bo := e.events(nx, KindBounced)[0]; bo.Class != "permanent" || bo.RuleID != "rfc5321.nxdomain" {
		t.Fatalf("nxdomain bounce: %+v", bo)
	}
	def := e.events(tmp, KindDeferred)
	if len(def) != 1 || def[0].RuleID != "rfc5321.dns_temporary" || def[0].Class != "transient" {
		t.Fatalf("temporary DNS: %+v", def)
	}
	if m := e.core.Message(tmp); m.State != store.StateQueued || m.NextAtMs != 60_000 || m.Attempts != 0 {
		t.Fatalf("temporary DNS message: %+v", m)
	}
	// The DNS recovers before the retry: the message is delivered then.
	e.host("flaky.test", "mx1.flaky.test", "127.0.1.30")
	e.run(time.Hour)
	e.allState(store.StateDelivered, tmp)
	if at := e.events(tmp, KindDelivered)[0].AtMs; at != 60_000 {
		t.Fatalf("delivered at %d, want 60000", at)
	}
}

func TestRetryScheduleExpiryAndDSN(t *testing.T) {
	e := newDeliveryEnv(t, testConfig()) // 60 s, x2, max 600 s, lifetime 3600 s
	defer451 := &Reply{Code: 451, Enhanced: "4.3.0", Text: "Requested action aborted: local error in processing"}
	h := e.host("always-defer.test", "mx1.always-defer.test", "127.0.1.6")
	h.dataEnd = func([]byte) *Reply { return defer451 }
	sender := e.host("sender.test", "mx1.sender.test", "127.0.1.40")
	id := e.ingest("acme", "app@sender.test", "user@always-defer.test")
	e.run(48 * time.Hour)

	var starts []int64
	for _, ev := range e.events(id, KindAttemptStarted) {
		starts = append(starts, ev.AtMs)
	}
	want := []int64{0, 60_000, 180_000, 420_000, 900_000, 1_500_000, 2_100_000, 2_700_000, 3_300_000}
	if !slices.Equal(starts, want) {
		t.Fatalf("attempts at %v, want %v", starts, want)
	}
	for i := 2; i < len(starts); i++ {
		if d, prev := starts[i]-starts[i-1], starts[i-1]-starts[max(i-2, 0)]; d < prev || d > 600_000 {
			t.Fatalf("interval %d after %d", d, prev)
		}
	}
	e.allState(store.StateExpired, id)
	exp := e.events(id, KindExpired)
	if len(exp) != 1 || exp[0].AtMs != 3_300_000 || exp[0].Code != 451 || exp[0].Attempt != 9 {
		t.Fatalf("expired: %+v", exp)
	}
	dsnEv := e.events(id, KindDSNEnqueued)
	if len(dsnEv) != 1 {
		t.Fatalf("dsn_enqueued: %+v", dsnEv)
	}
	var d struct {
		DSN string `json:"dsn_msg_id"`
	}
	json.Unmarshal([]byte(dsnEv[0].Detail), &d)
	e.allState(store.StateDelivered, d.DSN)
	if len(sender.got) != 1 || sender.got[0].from != "" || sender.got[0].to != "app@sender.test" || sender.got[0].id != d.DSN {
		t.Fatalf("DSN at the sender: %+v", sender.got)
	}
	checkDSN(t, sender.got[0].raw, id, "user@always-defer.test", "4.3.0")
}

// checkDSN parses a DSN as R-3464 §2 describes it.
func checkDSN(t *testing.T, raw []byte, origID, rcpt, status string) {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.Get("To") != "app@sender.test" || msg.Header.Get("Date") == "" {
		t.Fatalf("DSN headers: %v", msg.Header)
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/report" || params["report-type"] != "delivery-status" {
		t.Fatalf("DSN content type %s %v %v", mt, params, err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var types []string
	var status2 string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, p.Header.Get("Content-Type"))
		body, _ := io.ReadAll(p)
		if strings.HasPrefix(p.Header.Get("Content-Type"), "message/delivery-status") {
			status2 = string(body)
		}
	}
	if len(types) != 2 || !strings.HasPrefix(types[0], "text/plain") || types[1] != "message/delivery-status" {
		t.Fatalf("DSN parts: %v", types)
	}
	if err := bodyHas([]byte(status2), "Original-Envelope-Id: "+origID+"\r\n", "Reporting-MTA: dns; engine.mtaproof.test\r\n",
		"\r\n\r\nFinal-Recipient: rfc822; "+rcpt+"\r\n", "Action: failed\r\n", "Status: "+status+"\r\n"); err != nil {
		t.Fatalf("delivery-status: %v\n%s", err, status2)
	}
}

func TestNoDSNForNullReversePath(t *testing.T) {
	e := newDeliveryEnv(t, testConfig())
	defer451 := func([]byte) *Reply {
		return &Reply{Code: 451, Enhanced: "4.3.0", Text: "Requested action aborted: local error in processing"}
	}
	e.host("always-defer.test", "mx1.always-defer.test", "127.0.1.6").dataEnd = defer451
	e.host("sender.test", "mx1.sender.test", "127.0.1.40").dataEnd = defer451 // the DSN expires too
	id := e.ingest("acme", "app@sender.test", "user@always-defer.test")
	e.run(48 * time.Hour)
	e.allState(store.StateExpired, id, "m00000002")
	if dsn := e.core.Message("m00000002"); dsn.From != "" || dsn.To != "app@sender.test" {
		t.Fatalf("DSN envelope %q -> %q", dsn.From, dsn.To)
	}
	if n := len(e.events("", KindDSNEnqueued)); n != 1 {
		t.Fatalf("%d DSNs, want 1: no DSN for the expired DSN", n)
	}
	if e.core.Message("m00000003") != nil {
		t.Fatal("a DSN was created for a null reverse-path message")
	}
}

func TestSuppressionAfterNoEmail(t *testing.T) {
	e := newDeliveryEnv(t, testConfig())
	h := e.host("gmail-model.test", "mx1.gmail-model.test", "127.0.1.1")
	h.rcpt = func(to string) *Reply {
		if strings.HasPrefix(to, "gone@") {
			return &Reply{Code: 550, Enhanced: "5.1.1", Text: "The email account that you tried to reach does not exist."}
		}
		return nil
	}
	first := e.ingest("acme", "app@sender.test", "gone@gmail-model.test")
	e.run(time.Minute)
	e.allState(store.StateBounced, first)
	if bo := e.events(first, KindBounced)[0]; bo.Subtype != "NoEmail" || bo.Class != "permanent" || bo.Attempt != 1 {
		t.Fatalf("bounce: %+v", bo)
	}
	e.reopen() // the suppression is durable
	dials := len(e.net.dials)
	later := e.ingest("acme", "app@sender.test", "gone@GMAIL-MODEL.test")
	other := e.ingest("globex", "app@sender.test", "gone@gmail-model.test") // suppressions are per tenant
	e.run(time.Minute)
	e.allState(store.StateSuppressed, later)
	if got := e.kinds(later); got != "accepted,suppressed" {
		t.Fatalf("suppressed message events %s", got)
	}
	var d struct {
		By string `json:"suppressed_by"`
	}
	json.Unmarshal([]byte(e.events(later, KindSuppressed)[0].Detail), &d)
	if d.By != first {
		t.Fatalf("suppressed_by %q", d.By)
	}
	e.allState(store.StateBounced, other)
	if len(e.net.dials) != dials+1 {
		t.Fatalf("dials %v: the suppressed message must not connect", e.net.dials[dials:])
	}
}

func TestConnectionAndMessagesPerConnectionLimits(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.Domain = map[string]Limit{"busy.test": {Connections: 3, MsgsPerConn: 4}}
	e := newDeliveryEnv(t, cfg)
	h := e.host("busy.test", "mx1.busy.test", "127.0.1.50")
	var ids []string
	for i := 0; i < 30; i++ {
		ids = append(ids, e.ingest("acme", "app@sender.test", "u@busy.test"))
	}
	e.run(time.Minute)
	e.allState(store.StateDelivered, ids...)
	if h.maxOpen != 3 {
		t.Fatalf("max concurrent connections %d, want exactly the limit 3", h.maxOpen)
	}
	for i, n := range h.perConn {
		if n > 4 {
			t.Fatalf("connection %d carried %d messages, limit 4", i, n)
		}
	}
	if len(h.perConn) < 8 {
		t.Fatalf("%d connections for 30 messages at 4 per connection", len(h.perConn))
	}
	if e.net.open != 0 {
		t.Fatalf("%d connections left open", e.net.open)
	}
}

func TestSilentCloseLearnsMessagesPerConnection(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.Domain = map[string]Limit{"*": {Connections: 1}} // reuse one connection
	e := newDeliveryEnv(t, cfg)
	h := e.host("yahoo-model.test", "mx1.yahoo-model.test", "127.0.1.3")
	h.silentAfter = 5
	var ids []string
	for i := 0; i < 20; i++ {
		ids = append(ids, e.ingest("acme", "app@sender.test", "u@yahoo-model.test"))
	}
	e.run(0) // no backoff: everything goes at instant 0
	e.allState(store.StateDelivered, ids...)
	var closes []store.Event
	for _, ev := range e.events("", KindDeferred) {
		closes = append(closes, ev)
	}
	if len(closes) != 1 || closes[0].RuleID != "yahoo.silent_close" || closes[0].Provider != "yahoo-model" {
		t.Fatalf("deferred events: %+v", closes)
	}
	var d struct {
		Learned map[string]any `json:"learned"`
	}
	json.Unmarshal([]byte(closes[0].Detail), &d)
	if d.Learned["msgs_per_conn"] != float64(5) || d.Learned["key"] != "yahoo-model/127.0.2.1" {
		t.Fatalf("learned: %s", closes[0].Detail)
	}
	if m := e.core.Message(closes[0].MsgID); m.Retries != 0 || m.Attempts != 2 {
		t.Fatalf("silently closed message: retries %d attempts %d", m.Retries, m.Attempts)
	}
	for i, n := range h.perConn {
		if n > 5 {
			t.Fatalf("connection %d carried %d messages after learning 5", i, n)
		}
	}
}

func TestThrottleLowersLearnedRate(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.Domain = map[string]Limit{"*": {Connections: 1}} // one attempt in flight at a time
	e := newDeliveryEnv(t, cfg)
	h := e.host("yahoo-model.test", "mx1.yahoo-model.test", "127.0.1.3")
	throttled := 0
	// The receiver allows 10 MAILs per 60 s and answers the 3.2 reply
	// beyond that.
	h.mail = func(h *fakeHost, now int64) *Reply {
		n := 0
		for _, at := range h.mails {
			if at > now-60_000 {
				n++
			}
		}
		if n >= 10 {
			throttled++
			return &Reply{Code: 421, Enhanced: "4.7.0", Text: "[TSS04] Message temporarily deferred by Yahoo"}
		}
		return nil
	}
	var ids []string
	for i := 0; i < 30; i++ {
		ids = append(ids, e.ingest("acme", "app@sender.test", "u@yahoo-model.test"))
	}
	e.run(24 * time.Hour)
	e.allState(store.StateDelivered, ids...)
	if throttled != 1 {
		t.Fatalf("%d throttling replies, want 1: the engine did not slow down", throttled)
	}
	def := e.events("", KindDeferred)
	if len(def) != 1 || def[0].RuleID != "yahoo.tss04" || def[0].Code != 421 {
		t.Fatalf("deferred: %+v", def)
	}
	var d struct {
		Learned struct {
			Rate  int `json:"rate_limit"`
			Conns int `json:"connections"`
		} `json:"learned"`
	}
	json.Unmarshal([]byte(def[0].Detail), &d)
	if d.Learned.Rate != 5 || d.Learned.Conns != 1 {
		t.Fatalf("learned %s", def[0].Detail)
	}
	// After the throttle, no 60 s window holds more than the learned 5
	// attempt starts.
	var after []int64
	for _, ev := range e.events("", KindAttemptStarted) {
		if ev.Seq > def[0].Seq {
			after = append(after, ev.AtMs)
		}
	}
	for i := range after {
		n := 0
		for _, at := range after[i:] {
			if at < after[i]+60_000 {
				n++
			}
		}
		if n > 5 {
			t.Fatalf("%d attempts within 60 s from %d after learning 5", n, after[i])
		}
	}
}

func TestBlockedDestinationDoesNotHoldOthers(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.Domain = map[string]Limit{"slow.test": {Rate: &Rate{Limit: 1, PerS: 60}}}
	e := newDeliveryEnv(t, cfg)
	e.host("slow.test", "mx1.slow.test", "127.0.1.60")
	e.host("fast.test", "mx1.fast.test", "127.0.1.61")
	var slow, fast []string
	for i := 0; i < 5; i++ {
		slow = append(slow, e.ingest("acme", "app@sender.test", "u@slow.test"))
	}
	for i := 0; i < 5; i++ {
		fast = append(fast, e.ingest("acme", "app@sender.test", "u@fast.test"))
	}
	e.run(time.Hour)
	e.allState(store.StateDelivered, slow...)
	e.allState(store.StateDelivered, fast...)
	for _, id := range fast {
		if at := e.events(id, KindDelivered)[0].AtMs; at != 0 {
			t.Fatalf("fast.test message %s waited for slow.test: delivered at %d", id, at)
		}
	}
	for i, id := range slow {
		if at := e.events(id, KindAttemptStarted)[0].AtMs; at != int64(i)*60_000 {
			t.Fatalf("slow.test message %d started at %d, want %d", i, at, i*60_000)
		}
	}
}

func TestDeterministicEvents(t *testing.T) {
	trace := func(seed uint64) string {
		cfg := testConfig()
		cfg.Limits.Domain = map[string]Limit{"*": {Connections: 2, MsgsPerConn: 3}}
		e := newDeliveryEnv(t, cfg)
		e.seed = seed
		e.reopen()
		for _, n := range []string{"a", "b", "c"} {
			e.net.add(&fakeHost{name: "mx." + n + ".test"}, "127.0.1."+map[string]string{"a": "71", "b": "72", "c": "73"}[n])
		}
		e.res.answers["multi.test"] = Resolution{Status: ResolvedOK, MX: []MXHost{
			mx(10, "mx.a.test", "127.0.1.71"), mx(10, "mx.b.test", "127.0.1.72"), mx(10, "mx.c.test", "127.0.1.73")}}
		for i := 0; i < 12; i++ {
			e.ingest("acme", "app@sender.test", "u@multi.test")
		}
		e.run(time.Hour)
		var b strings.Builder
		for _, ev := range e.events("", "") {
			j, _ := json.Marshal(ev)
			b.Write(j)
		}
		return b.String()
	}
	a, b := trace(1), trace(1)
	if a != b {
		t.Fatal("same seed gave different event logs")
	}
	other := ""
	for seed := uint64(2); seed < 10 && (other == "" || other == a); seed++ {
		other = trace(seed)
	}
	if other == a {
		t.Fatal("eight other seeds gave the same MX choice as seed 1")
	}
}
