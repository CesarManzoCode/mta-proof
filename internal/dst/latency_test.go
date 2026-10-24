package dst

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// slowLab builds a repository root with one test profile, slow, whose
// latency rules delay the greeting by connectMs and the reply to DATA by
// dataEndMs; it reuses lab/engine.json.
func slowLab(t *testing.T, connectMs, dataEndMs int) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("profiles/slow.json", fmt.Sprintf(`{"name": "slow", "sources": ["R-5321"], "adaptation_period_s": 0,
	  "hosts": ["mx1.slow.test"],
	  "rules": [
	    {"id": "slow.connect", "kind": "latency", "params": {"stage": "connect", "ms": %d}, "assumption": true},
	    {"id": "slow.data_end", "kind": "latency", "params": {"stage": "data_end", "ms": %d}, "assumption": true}
	  ]}`, connectMs, dataEndMs))
	write("lab/zone.json", `{"ttl": 300, "records": [
	  {"name": "slow.test", "type": "MX", "pref": 10, "host": "mx1.slow.test"},
	  {"name": "mx1.slow.test", "type": "A", "addr": "127.0.1.9"}]}`)
	eng, err := os.ReadFile(filepath.Join(root, "lab", "engine.json"))
	if err != nil {
		t.Fatal(err)
	}
	write("lab/engine.json", string(eng))
	return dir
}

func slowScenario(t *testing.T, messages int, durationS float64) *Scenario {
	t.Helper()
	sc, err := ParseScenario(fmt.Appendf(nil, `{"name": "slow", "profiles": ["slow"],
	  "tenants": [{"name": "t", "from": "a@slow.test"}], "domains": [{"domain": "slow.test"}],
	  "messages": %d, "arrival": {"process": "poisson", "rate_per_s": 50}, "duration_s": %g}`, messages, durationS))
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func runSlow(t *testing.T, base string, sc *Scenario) (string, *Summary) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "run")
	sum, err := Run(Options{Root: base, Scenario: sc, Seed: 1, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	return out, sum
}

// TestLatencyAdvancesVirtualClockPerConnection checks that profile latency
// is spent in virtual time by the connection that waits for the reply,
// while other connections keep going.
func TestLatencyAdvancesVirtualClockPerConnection(t *testing.T) {
	base := slowLab(t, 500, 2000)
	dir, sum := runSlow(t, base, slowScenario(t, 2, 600))
	if !sum.Complete || sum.States["delivered"] != 2 {
		t.Fatalf("summary %+v", sum)
	}
	evs := readReceiver(t, dir)
	byConn := map[int64][]recvEvent{}
	for _, e := range evs {
		byConn[e.Conn] = append(byConn[e.Conn], e)
	}
	if len(byConn) != 2 {
		t.Fatalf("%d connections, want 2 (one per message, opened at once)", len(byConn))
	}
	at := func(conn int64, ev string) int64 {
		for _, e := range byConn[conn] {
			if e.Ev == ev {
				return e.AtMs
			}
		}
		t.Fatalf("conn %d has no %s", conn, ev)
		return 0
	}
	for conn := int64(1); conn <= 2; conn++ {
		if d := at(conn, "ehlo") - at(conn, "connect"); d != 500 {
			t.Errorf("conn %d: EHLO %d ms after connect, want the 500 ms greeting latency", conn, d)
		}
	}
	// Both connections waited at the same time: the second one connected
	// before the first one got its greeting.
	if at(2, "connect") >= at(1, "ehlo") {
		t.Errorf("conn 2 connected at %d, after conn 1 sent EHLO at %d", at(2, "connect"), at(1, "ehlo"))
	}
	delivered := map[string]int64{}
	for _, e := range readEvents(t, dir) {
		if e.Kind == "delivered" {
			delivered[e.MsgID] = e.AtMs
		}
	}
	for _, e := range evs {
		if e.Ev == "data_end" {
			if got := delivered[e.MsgID]; got != e.AtMs+2000 {
				t.Errorf("%s: data_end at %d, delivered at %d; want the 2000 ms reply latency", e.MsgID, e.AtMs, got)
			}
		}
	}
}

// TestRunEndingMidReply ends the run while a reply is delayed: the
// connection is dropped at shutdown and the attempt stays inflight, as
// after a crash, without blocking the run.
func TestRunEndingMidReply(t *testing.T) {
	base := slowLab(t, 0, 10000)
	sc := slowScenario(t, 1, 1)
	first := sc.Arrivals(1)[0].AtMs
	sc.DurationS = float64(first+5000) / 1000
	dir, sum := runSlow(t, base, sc)
	if sum.Complete || sum.States["inflight"] != 1 {
		t.Fatalf("summary %+v", sum)
	}
	evs := readReceiver(t, dir)
	last := evs[len(evs)-1]
	if last.Ev != "drop" || last.Rule != "shutdown" {
		t.Errorf("last receiver event %+v, want a drop by shutdown", last)
	}
	kinds := []string{}
	for _, e := range readEvents(t, dir) {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, ","); got != "accepted,attempt_started,routed" {
		t.Errorf("events %s", got)
	}
}
