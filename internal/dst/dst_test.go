package dst

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CesarManzoCode/mta-proof/internal/store"
)

const root = "../.."

// runScenario runs lab/scenarios/<name>.json into a new directory.
func runScenario(t *testing.T, name string, seed uint64) (string, *Summary) {
	t.Helper()
	sc, err := LoadScenario(filepath.Join(root, "lab", "scenarios", name+".json"), "")
	if err != nil {
		t.Fatal(err)
	}
	return runSc(t, sc, seed, root)
}

func runSc(t *testing.T, sc *Scenario, seed uint64, base string) (string, *Summary) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "run")
	sum, err := Run(Options{Root: base, Research: "vault/RESEARCH.md", Scenario: sc, Seed: seed, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	return out, sum
}

type recvEvent struct {
	Seq      int64   `json:"seq"`
	AtMs     int64   `json:"at_ms"`
	Conn     int64   `json:"conn"`
	Host     string  `json:"host"`
	Profile  string  `json:"profile"`
	Ev       string  `json:"ev"`
	MailFrom *string `json:"mail_from"`
	MsgID    string  `json:"msg_id"`
	Code     int     `json:"code"`
	Rule     string  `json:"rule"`
	Counted  *bool   `json:"counted"`
}

func readReceiver(t *testing.T, dir string) []recvEvent {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "receiver.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []recvEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var e recvEvent
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func readEvents(t *testing.T, dir string) []store.Event {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "engine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	evs, err := st.Events()
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// traceHash recomputes contract 3.6 from the files, without TraceHash.
func traceHash(t *testing.T, dir string) string {
	t.Helper()
	var all []byte
	for _, name := range []string{"receiver.jsonl", "events.jsonl"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	sum := sha256.Sum256(all)
	return hex.EncodeToString(sum[:])
}

func storedHash(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "trace.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestDeterminism(t *testing.T) {
	a, _ := runScenario(t, "determinism", 1)
	b, _ := runScenario(t, "determinism", 1)
	o, _ := runScenario(t, "determinism", 2)
	var hashes []string
	for _, dir := range []string{a, b, o} {
		h := traceHash(t, dir)
		if s := storedHash(t, dir); s != h {
			t.Fatalf("%s: trace.sha256 %s, recomputed %s", dir, s, h)
		}
		hashes = append(hashes, h)
	}
	if hashes[0] != hashes[1] {
		t.Errorf("same seed, different traces: %s %s", hashes[0], hashes[1])
	}
	if hashes[0] == hashes[2] {
		t.Errorf("another seed gave the same trace %s", hashes[0])
	}
	for _, name := range []string{"engine.db", "messages"} {
		if _, err := os.Stat(filepath.Join(a, name)); err != nil {
			t.Error(err)
		}
	}
	// The scenario covers what it claims: every terminal state, the
	// no-reply decisions and a DSN.
	seen := map[string]bool{}
	for _, e := range readEvents(t, a) {
		seen[e.Kind] = true
		seen[e.RuleID] = true
	}
	for _, want := range []string{"delivered", "bounced", "expired", "suppressed", "dsn_enqueued",
		"rfc7505.null_mx", "rfc5321.nxdomain", "google.no_account"} {
		if !seen[want] {
			t.Errorf("determinism scenario never produced %s", want)
		}
	}
}

func TestBackoffSimulatesDaysInSeconds(t *testing.T) {
	start := time.Now()
	dir, sum := runScenario(t, "backoff", 1)
	wall := time.Since(start)
	sc, _ := LoadScenario(filepath.Join(root, "lab", "scenarios", "backoff.json"), "")
	cfg, err := sc.EngineConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	lifeMs, maxMs := cfg.Retry.LifetimeMs(), cfg.Retry.DelayMs(1000)
	t.Logf("backoff: %d virtual ms in %v of wall time; %+v", sum.VirtualMs, wall, sum)
	if !sum.Complete {
		t.Error("the run ended with events still queued")
	}
	if wall > 30*time.Second {
		t.Errorf("simulating %v took %v", time.Duration(sum.VirtualMs)*time.Millisecond, wall)
	}
	attempts := map[string][]int64{}
	nullSender := 0
	for _, e := range readReceiver(t, dir) {
		if e.Ev == "mail" && e.MailFrom != nil && *e.MailFrom == "" {
			nullSender++
		}
		if e.Ev == "data_end" && e.Profile == "always-defer" {
			attempts[e.MsgID] = append(attempts[e.MsgID], e.AtMs)
		}
	}
	if len(attempts) != sc.Messages {
		t.Fatalf("%d messages reached always-defer, want %d", len(attempts), sc.Messages)
	}
	accepted, expired, dsn := map[string]int64{}, map[string]int64{}, map[string]bool{}
	for _, e := range readEvents(t, dir) {
		switch e.Kind {
		case "accepted":
			accepted[e.MsgID] = e.AtMs
		case "expired":
			expired[e.MsgID] = e.AtMs
		case "dsn_enqueued":
			dsn[e.MsgID] = true
		}
	}
	for id, ts := range attempts {
		if len(ts) < 3 {
			t.Errorf("%s: %d attempts", id, len(ts))
		}
		for i := 2; i < len(ts); i++ {
			if g1, g2 := ts[i-1]-ts[i-2], ts[i]-ts[i-1]; g2 < g1 || g2 > maxMs {
				t.Errorf("%s: interval %d ms after %d ms (max %d)", id, g2, g1, maxMs)
			}
		}
		if ts[0] != accepted[id] {
			t.Errorf("%s: first attempt at %d, accepted at %d", id, ts[0], accepted[id])
		}
		if last := ts[len(ts)-1]; last > accepted[id]+lifeMs {
			t.Errorf("%s: attempt at %d after the lifetime", id, last)
		}
		at, ok := expired[id]
		if !ok || !dsn[id] {
			t.Errorf("%s: expired %v, dsn %v", id, ok, dsn[id])
		}
		// The message lived (almost) its whole lifetime in virtual time:
		// it expires when the next retry would fall after it.
		if at-accepted[id] < lifeMs-maxMs {
			t.Errorf("%s: expired after %d ms of a %d ms lifetime", id, at-accepted[id], lifeMs)
		}
	}
	if nullSender != sc.Messages {
		t.Errorf("%d MAIL FROM:<> at the receiver, want one DSN per message (%d)", nullSender, sc.Messages)
	}
}

func TestLimitsScenarioHasNoCountedViolations(t *testing.T) {
	if testing.Short() {
		t.Skip("10 800 messages")
	}
	dir, sum := runScenario(t, "limits", 1)
	t.Logf("limits: %+v", sum)
	profiles := map[string]bool{}
	uncounted := 0
	for _, e := range readReceiver(t, dir) {
		profiles[e.Profile] = true
		if e.Ev == "violation" {
			if *e.Counted {
				t.Errorf("counted violation of %s at %d ms on conn %d", e.Rule, e.AtMs, e.Conn)
			} else {
				uncounted++
			}
		}
	}
	for _, p := range []string{"gmail-model", "yahoo-model", "outlook-model"} {
		if !profiles[p] {
			t.Errorf("profile %s never reached", p)
		}
	}
	// The scenario must push the engine into the profiles' limits, or it
	// shows nothing about adaptation.
	if uncounted == 0 {
		t.Error("no violation at all: the scenario does not reach any limit")
	}
	if sum.States["delivered"] != sum.Messages {
		t.Errorf("states %v", sum.States)
	}
}

// TestEventsMatchPython rebuilds events.jsonl from engine.db with the
// standard library of Python, as the verifier can.
func TestEventsMatchPython(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	dir, _ := runScenario(t, "determinism", 3)
	script := `import json, sqlite3, sys
con = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
con.row_factory = sqlite3.Row
for r in con.execute("SELECT * FROM events ORDER BY seq"):
    sys.stdout.write(json.dumps(dict(r), sort_keys=True, separators=(",", ":")) + "\n")
`
	want, err := exec.Command(py, "-I", "-c", script, filepath.Join(dir, "engine.db")).Output()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("events.jsonl differs from the Python export (%d vs %d bytes)", len(got), len(want))
	}
}

func TestAppendStringLikePython(t *testing.T) {
	// json.dumps of the same string in Python 3.
	in := "a\"b\\c\nd\re\tf\bg\fh\x00i\x1fj\x7fk<l>&mñ€😀"
	want := `"a\"b\\c\nd\re\tf\bg\fh\u0000i\u001fj\u007fk<l>&m\u00f1\u20ac\ud83d\ude00"`
	if got := string(appendString(nil, in)); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestClassifyCorpus(t *testing.T) {
	out := filepath.Join(t.TempDir(), "decisions.jsonl")
	n, err := ClassifyCorpus(filepath.Join(root, "corpus", "classification.json"), out)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "corpus", "classification.json"))
	var cases []CorpusCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if n != len(cases) || len(lines) != len(cases) {
		t.Fatalf("%d decisions in %d lines for %d cases", n, len(lines), len(cases))
	}
	for i, line := range lines {
		var d map[string]string
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatal(err)
		}
		c := cases[i]
		if len(d) != 4 || d["id"] != c.ID || d["rule_id"] == "" {
			t.Errorf("line %d: %s", i+1, line)
		}
		if d["class"] != c.ExpectedClass || d["subtype"] != c.ExpectedSubtype {
			t.Errorf("%s: %s/%s, expected %s/%s", c.ID, d["class"], d["subtype"], c.ExpectedClass, c.ExpectedSubtype)
		}
	}
}

func TestScenarioRejectsUnknownFieldsAndMergesEngine(t *testing.T) {
	base := `{"name":"x","profiles":["baseline"],"tenants":[{"name":"t","from":"a@baseline.test"}],
		"domains":[{"domain":"baseline.test"}],"messages":1,"arrival":{"process":"poisson","rate_per_s":1},
		"duration_s":10%s}`
	if _, err := ParseScenario([]byte(strings.Replace(base, "%s", `,"bogus":1`, 1))); err == nil {
		t.Error("unknown field accepted")
	}
	if _, err := ParseScenario([]byte(strings.Replace(strings.Replace(base, "%s", "", 1), "poisson", "uniform", 1))); err == nil {
		t.Error("unknown arrival process accepted")
	}
	sc, err := ParseScenario([]byte(strings.Replace(base, "%s",
		`,"engine":{"set":{"retry_policy":{"initial_s":5},"limits":{"domain":{"x.test":{"connections":1}}}}}`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := sc.EngineConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Retry.InitialS != 5 || cfg.Retry.LifetimeS != 432000 || cfg.Limits.Domain["x.test"].Connections != 1 ||
		cfg.Limits.Domain["*"].Connections != 5 {
		t.Errorf("merged configuration %+v %+v", cfg.Retry, cfg.Limits.Domain)
	}
	if a, b := sc.Arrivals(1), sc.Arrivals(1); a[0] != b[0] {
		t.Errorf("arrivals differ for the same seed: %+v %+v", a[0], b[0])
	}
}

func TestScenarioVariantsAndTenantDomains(t *testing.T) {
	data := []byte(`{"name":"v","profiles":["gmail-model"],
		"tenants":[{"name":"good","from":"a@baseline.test","weight":2},
			{"name":"bad","from":"b@baseline.test","domains":[{"domain":"gmail-model.test","local_prefix":"nouser"}]}],
		"domains":[{"domain":"baseline.test"}],"messages":200,"arrival":{"process":"poisson","rate_per_s":1},
		"duration_s":10,
		"variants":{"healthy":{"tenants":[{"name":"good","from":"a@baseline.test","weight":2},
			{"name":"bad","from":"b@baseline.test","domains":[{"domain":"gmail-model.test"}]}]}}}`)
	sc, err := ParseScenario(data)
	if err != nil {
		t.Fatal(err)
	}
	v, err := sc.BuildVariant(data, "healthy")
	if err != nil {
		t.Fatal(err)
	}
	if v.Variant != "healthy" || v.Name != "v" || v.Variants != nil {
		t.Fatalf("variant %q of %q, variants %v", v.Variant, v.Name, v.Variants)
	}
	// Same draws: the variant changes only the bad tenant's local part.
	a, b := sc.Arrivals(7), v.Arrivals(7)
	bad := 0
	for i := range a {
		want := a[i].Sub
		if want.Tenant == "bad" {
			bad++
			if !strings.HasPrefix(want.To, "nouser") || !strings.HasSuffix(want.To, "@gmail-model.test") {
				t.Fatalf("bad tenant message to %s", want.To)
			}
			want.To = strings.TrimPrefix(want.To, "no")
		} else if !strings.HasSuffix(want.To, "@baseline.test") {
			t.Fatalf("good tenant message to %s", want.To)
		}
		if a[i].AtMs != b[i].AtMs || want != b[i].Sub {
			t.Fatalf("message %d: %+v / %+v", i, a[i], b[i])
		}
	}
	if bad == 0 || bad == len(a) {
		t.Fatalf("%d of %d messages from the bad tenant", bad, len(a))
	}
	if _, err := sc.BuildVariant(data, "missing"); err == nil {
		t.Error("unknown variant built")
	}
	for _, bogus := range []string{`{"name":"w"}`, `{"variants":{}}`, `{"messages":0}`} {
		d := []byte(strings.Replace(string(data), `"variants":{`, `"variants":{"x":`+bogus+`,`, 1))
		if _, err := ParseScenario(d); err == nil {
			t.Errorf("variant %s accepted", bogus)
		}
	}
}

func TestRunRefusesNonEmptyOut(t *testing.T) {
	sc, err := LoadScenario(filepath.Join(root, "lab", "scenarios", "backoff.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	os.WriteFile(filepath.Join(out, "receiver.jsonl"), []byte("old\n"), 0o644)
	if _, err := Run(Options{Root: root, Scenario: sc, Seed: 1, Out: out}); err == nil {
		t.Fatal("run over an existing run directory")
	}
}
