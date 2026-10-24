package mtaq

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/CesarManzoCode/mta-proof/internal/store"
)

// smallDB writes a small events log through the engine's store and closes
// it, so mtaq reads a file the way it reads a finished run.
//
//	m1: deferred 451, then interrupted by a kill, then delivered.
//	m2: null MX, bounced without an attempt.
//	m3: deferred, then expired for its lifetime without an attempt.
//	m4: an attempt still open (inflight) from another address.
//	and one tenant_paused event without msg_id.
func smallDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "engine.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	base := store.Event{Tenant: "acme", Domain: "gmail-model.test", MsgType: "transactional"}
	route := func(e store.Event) store.Event {
		e.Provider, e.MXHost, e.SrcIP = "gmail-model", "mx1.gmail-model.test", "127.0.2.1"
		return e
	}
	ev := func(at int64, id, kind string, f func(*store.Event)) store.Write {
		e := base
		e.AtMs, e.MsgID, e.Kind = at, id, kind
		if f != nil {
			f(&e)
		}
		return store.Write{Event: &e}
	}
	writes := []store.Write{
		ev(10, "m1", "accepted", func(e *store.Event) { e.Detail = `{"payload_sha256":"aa"}` }),
		ev(11, "m1", "attempt_started", func(e *store.Event) {
			*e = route(*e)
			e.MXHost, e.Attempt = "", 1
		}),
		ev(12, "m1", "tls_negotiated", func(e *store.Event) {
			*e = route(*e)
			e.Attempt, e.Detail = 1, `{"conn":1,"policy":"opportunistic","verified":true}`
		}),
		ev(12, "m1", "routed", func(e *store.Event) {
			*e = route(*e)
			e.Attempt, e.Detail = 1, `{"addr":"127.0.1.1:25","conn":1,"mx":["mx1.gmail-model.test"]}`
		}),
		ev(13, "m1", "classified", func(e *store.Event) {
			*e = route(*e)
			e.Attempt, e.Code, e.Enhanced, e.Text = 1, 451, "4.3.0", "Requested action aborted: local error in processing"
			e.Class, e.Subtype, e.RuleID = "transient", "General", "rfc3463.class4"
		}),
		ev(13, "m1", "deferred", func(e *store.Event) {
			*e = route(*e)
			e.Attempt, e.Code, e.Enhanced, e.Text = 1, 451, "4.3.0", "Requested action aborted: local error in processing"
			e.Class, e.Subtype, e.RuleID = "transient", "General", "rfc3463.class4"
			e.Detail = `{"conn":1,"stage":"data_end"}`
		}),
		ev(20, "m1", "attempt_started", func(e *store.Event) { *e = route(*e); e.Attempt = 2 }),
		ev(20, "m1", "routed", func(e *store.Event) {
			*e = route(*e)
			e.Attempt, e.Detail = 2, `{"conn":1,"reused":true}`
		}),
		ev(30, "", "tenant_paused", func(e *store.Event) {
			e.Domain, e.MsgType, e.Detail = "", "", `{"bounce_rate":0.5}`
		}),
		ev(40, "m1", "recovered", func(e *store.Event) { e.Attempt, e.Detail = 2, `{"from_state":"inflight"}` }),
		ev(41, "m1", "attempt_started", func(e *store.Event) { *e = route(*e); e.Attempt = 3 }),
		ev(41, "m1", "routed", func(e *store.Event) {
			*e = route(*e)
			e.Attempt, e.Detail = 3, `{"addr":"127.0.1.1:25","conn":7,"mx":["mx1.gmail-model.test"]}`
		}),
		ev(42, "m1", "delivered", func(e *store.Event) {
			*e = route(*e)
			e.Attempt, e.Code, e.Enhanced, e.Text, e.Detail = 3, 250, "2.0.0", "OK", `{"conn":7}`
		}),
		ev(50, "m2", "accepted", func(e *store.Event) { e.Domain = "nullmx.test" }),
		ev(50, "m2", "routed", func(e *store.Event) { e.Domain, e.Detail = "nullmx.test", `{"null_mx":true}` }),
		ev(50, "m2", "classified", func(e *store.Event) {
			e.Domain, e.Code, e.Enhanced, e.Class, e.Subtype, e.RuleID = "nullmx.test", 556, "5.1.10", "permanent", "NullMX", "rfc7505.null_mx"
		}),
		ev(50, "m2", "bounced", func(e *store.Event) {
			e.Domain, e.Code, e.Enhanced, e.Class, e.Subtype, e.RuleID = "nullmx.test", 556, "5.1.10", "permanent", "NullMX", "rfc7505.null_mx"
		}),
		ev(60, "m3", "accepted", nil),
		ev(61, "m3", "attempt_started", func(e *store.Event) { *e = route(*e); e.Attempt = 1 }),
		ev(62, "m3", "deferred", func(e *store.Event) {
			*e = route(*e)
			e.Attempt, e.Class, e.Subtype, e.RuleID = 1, "transient", "Connection", "engine.no_reply"
			e.Detail = `{"conn":3,"error":"EOF","stage":"data_end"}`
		}),
		ev(90, "m3", "expired", func(e *store.Event) { e.Attempt, e.Detail = 1, `{"reason":"lifetime"}` }),
		ev(95, "m4", "accepted", func(e *store.Event) { e.MsgType = "marketing" }),
		ev(96, "m4", "attempt_started", func(e *store.Event) {
			*e = route(*e)
			e.MsgType, e.SrcIP, e.Attempt = "marketing", "127.0.2.2", 1
		}),
	}
	if err := s.Apply(writes); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Main(args, &out, &errb)
	return code, out.String(), errb.String()
}

// decode reads a timeline the way a user of the tool does: from its JSON.
func decode(t *testing.T, line string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(line), &v); err != nil {
		t.Fatalf("%v: %q", err, line)
	}
	return v
}

func TestTimelineIsCausal(t *testing.T) {
	db := smallDB(t)
	code, out, errs := run(t, "timeline", "--db", db, "--msg", "m1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	tl := decode(t, out)
	if tl["msg_id"] != "m1" || tl["state"] != "delivered" || tl["accepted_at_ms"] != 10.0 ||
		tl["tenant"] != "acme" || tl["domain"] != "gmail-model.test" {
		t.Fatalf("header: %v", tl)
	}
	events := tl["events"].([]any)
	if len(events) != 12 {
		t.Fatalf("%d events, want the 12 of m1 (not the tenant event)", len(events))
	}
	var seqs []float64
	for _, e := range events {
		e := e.(map[string]any)
		if len(e) != len(store.EventColumns) {
			t.Fatalf("event with %d columns: %v", len(e), e)
		}
		seqs = append(seqs, e["seq"].(float64))
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("events not in seq order: %v", seqs)
		}
	}
	if d := events[0].(map[string]any)["detail"]; !reflect.DeepEqual(d, map[string]any{"payload_sha256": "aa"}) {
		t.Fatalf("detail not decoded: %#v", d)
	}
	if v := events[1].(map[string]any)["mx_host"]; v != nil {
		t.Fatalf("NULL column printed as %v", v)
	}

	attempts := tl["attempts"].([]any)
	type want struct {
		attempt              float64
		result, stage, src   any
		code, conn, started  any
		ended, addr, rule_id any
		reused               bool
		tls                  int
	}
	wants := []want{
		{1, "deferred", "data_end", "127.0.2.1", 451.0, 1.0, 11.0, 13.0, "127.0.1.1:25", "rfc3463.class4", false, 1},
		{2, "interrupted", nil, "127.0.2.1", nil, 1.0, 20.0, 40.0, nil, nil, true, 0},
		{3, "delivered", "data_end", "127.0.2.1", 250.0, 7.0, 41.0, 42.0, "127.0.1.1:25", nil, false, 0},
	}
	if len(attempts) != len(wants) {
		t.Fatalf("%d attempts: %v", len(attempts), attempts)
	}
	for i, w := range wants {
		a := attempts[i].(map[string]any)
		got := want{a["attempt"].(float64), a["result"], a["stage"], a["src_ip"], a["code"], a["conn"],
			a["started_at_ms"], a["ended_at_ms"], a["addr"], a["rule_id"], a["reused"].(bool), len(a["tls"].([]any))}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("attempt %d:\n got %+v\nwant %+v", i+1, got, w)
		}
		if a["mx_host"] != "mx1.gmail-model.test" || a["provider"] != "gmail-model" {
			t.Errorf("attempt %d route: %v %v", i+1, a["mx_host"], a["provider"])
		}
	}
}

func TestTimelineMessageLevelResults(t *testing.T) {
	db := smallDB(t)
	_, out, _ := run(t, "timeline", "--db", db, "--msg", "m2")
	tl := decode(t, out)
	if tl["state"] != "bounced" || len(tl["attempts"].([]any)) != 0 {
		t.Fatalf("null MX: state %v, attempts %v", tl["state"], tl["attempts"])
	}
	_, out, _ = run(t, "timeline", "--db", db, "--msg", "m3")
	tl = decode(t, out)
	as := tl["attempts"].([]any)
	if tl["state"] != "expired" || len(as) != 1 {
		t.Fatalf("lifetime expiry: state %v, %d attempts", tl["state"], len(as))
	}
	a := as[0].(map[string]any)
	if a["result"] != "deferred" || a["stage"] != "data_end" || a["code"] != nil || a["error"] != "EOF" {
		t.Fatalf("no-reply attempt: %v", a)
	}
	_, out, _ = run(t, "timeline", "--db", db, "--msg", "m4")
	tl = decode(t, out)
	as = tl["attempts"].([]any)
	if tl["state"] != "inflight" || len(as) != 1 || as[0].(map[string]any)["result"] != nil {
		t.Fatalf("open attempt: %v", tl)
	}
}

func TestTimelineAllAndFilters(t *testing.T) {
	db := smallDB(t)
	ids := func(args ...string) []string {
		t.Helper()
		code, out, errs := run(t, append([]string{"timeline", "--db", db, "--all"}, args...)...)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errs)
		}
		var got []string
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if line != "" {
				got = append(got, decode(t, line)["msg_id"].(string))
			}
		}
		return got
	}
	cases := []struct {
		args []string
		want []string
	}{
		{nil, []string{"m1", "m2", "m3", "m4"}},
		{[]string{"--domain", "nullmx.test"}, []string{"m2"}},
		{[]string{"--src-ip", "127.0.2.2"}, []string{"m4"}},
		{[]string{"--msg-type", "marketing"}, []string{"m4"}},
		{[]string{"--kind", "recovered"}, []string{"m1"}},
		{[]string{"--provider", "gmail-model", "--kind", "delivered"}, []string{"m1"}},
		{[]string{"--tenant", "other"}, nil},
	}
	for _, c := range cases {
		if got := ids(c.args...); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v: got %v, want %v", c.args, got, c.want)
		}
	}
	// A filtered timeline still has every event of the message.
	_, out, _ := run(t, "timeline", "--db", db, "--msg", "m1", "--kind", "recovered")
	if n := len(decode(t, out)["events"].([]any)); n != 12 {
		t.Fatalf("filtered timeline has %d events", n)
	}
	if code, _, _ := run(t, "timeline", "--db", db, "--msg", "m1", "--kind", "bounced"); code != 1 {
		t.Fatalf("non-matching --msg: exit %d, want 1", code)
	}
	if code, _, _ := run(t, "timeline", "--db", db, "--msg", "m9"); code != 1 {
		t.Fatalf("unknown message: exit %d, want 1", code)
	}
}

func agg(t *testing.T, args ...string) []map[string]any {
	t.Helper()
	code, out, errs := run(t, append([]string{"agg"}, args...)...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("%v: %q", err, out)
	}
	return rows
}

func TestAgg(t *testing.T) {
	db := smallDB(t)
	rows := agg(t, "--db", db, "--by", "kind")
	counts := map[string]float64{}
	total := 0.0
	for _, r := range rows {
		if len(r) != 2 {
			t.Fatalf("row with keys other than kind and count: %v", r)
		}
		counts[r["kind"].(string)] = r["count"].(float64)
		total += r["count"].(float64)
	}
	if total != 23 || counts["accepted"] != 4 || counts["attempt_started"] != 5 || counts["tenant_paused"] != 1 {
		t.Fatalf("by kind: %v (total %v)", counts, total)
	}

	rows = agg(t, "--db", db, "--by", "tenant,src_ip", "--kind", "attempt_started")
	want := []map[string]any{
		{"tenant": "acme", "src_ip": "127.0.2.1", "count": 4.0},
		{"tenant": "acme", "src_ip": "127.0.2.2", "count": 1.0},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("by tenant,src_ip: %v", rows)
	}

	// NULL is a group of its own and sorts first, as in SQLite.
	rows = agg(t, "--db", db, "--by", "provider", "--tenant", "acme")
	if rows[0]["provider"] != nil || rows[1]["provider"] != "gmail-model" || len(rows) != 2 ||
		rows[0]["count"].(float64)+rows[1]["count"].(float64) != 23 {
		t.Fatalf("by provider: %v", rows)
	}
	if rows := agg(t, "--db", db, "--by", "domain", "--msg-type", "none"); len(rows) != 0 {
		t.Fatalf("empty filter result: %v", rows)
	}
	rows = agg(t, "--db", db, "--by", strings.Join(Dimensions, ","))
	sum := 0.0
	for _, r := range rows {
		if len(r) != len(Dimensions)+1 {
			t.Fatalf("row %v", r)
		}
		sum += r["count"].(float64)
	}
	if sum != 23 {
		t.Fatalf("all dimensions sum to %v", sum)
	}
}

func TestUsage(t *testing.T) {
	db := smallDB(t)
	for _, args := range [][]string{
		{},
		{"what"},
		{"timeline", "--msg", "m1"},
		{"timeline", "--db", db},
		{"timeline", "--db", db, "--all", "--msg", "m1"},
		{"agg", "--db", db},
		{"agg", "--db", db, "--by", "kind,colour"},
		{"agg", "--db", db, "--by", "kind,kind"},
		{"agg", "--db", db, "--by", "kind", "extra"},
		{"agg", "--db", db, "--by", "kind", "--nope", "x"},
	} {
		if code, _, _ := run(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	missing := filepath.Join(t.TempDir(), "engine.db")
	if code, _, _ := run(t, "agg", "--db", missing, "--by", "kind"); code != 1 {
		t.Fatalf("missing database: exit %d, want 1", code)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("mtaq created %s", missing)
	}
}

func TestReadOnly(t *testing.T) {
	db := smallDB(t)
	read := func(name string) []byte {
		b, err := os.ReadFile(name)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return b
	}
	before := sha256.Sum256(read(db))
	run(t, "timeline", "--db", db, "--all")
	run(t, "agg", "--db", db, "--by", "kind")
	if after := sha256.Sum256(read(db)); after != before {
		t.Fatal("engine.db changed")
	}
	// A reader of a WAL database may create the -wal and -shm files; the
	// log it would write to must stay empty.
	if wal := read(db + "-wal"); len(wal) != 0 {
		t.Fatalf("mtaq wrote %d bytes to the WAL", len(wal))
	}
	h, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.Exec("INSERT INTO events (kind) VALUES ('x')"); err == nil {
		t.Fatal("insert through mtaq's handle succeeded")
	}
	if _, err := h.Exec("PRAGMA query_only = 0"); err == nil {
		if _, err := h.Exec("INSERT INTO events (kind) VALUES ('x')"); err == nil {
			t.Fatal("insert succeeded after turning query_only off: mode=ro is not in effect")
		}
	}
}
