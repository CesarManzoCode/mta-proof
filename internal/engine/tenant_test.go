package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/CesarManzoCode/mta-proof/internal/store"
)

func TestTenantMonitorWindow(t *testing.T) {
	m := &tenantState{ring: make([]bool, 4)}
	// Three bounces in a window of four that is not full yet: no crossing.
	for i := 0; i < 3; i++ {
		m.add(true)
		if m.crossed(0.25) {
			t.Fatalf("crossed with %d of 4 results", m.n)
		}
	}
	m.add(false)
	if !m.crossed(0.25) || m.bounced != 3 || m.rate() != 0.75 {
		t.Fatalf("full window 3/4: crossed %v, bounced %d, rate %v", m.crossed(0.25), m.bounced, m.rate())
	}
	// The window slides: four deliveries push the bounces out.
	for i := 0; i < 3; i++ {
		m.add(false)
	}
	if m.bounced != 0 || m.crossed(0.25) {
		t.Fatalf("after sliding: bounced %d", m.bounced)
	}
	// The threshold is strict: exactly 1/4 does not cross 0.25.
	m.add(true)
	if m.bounced != 1 || m.crossed(0.25) || !m.crossed(0.24) {
		t.Fatalf("1/4 against 0.25 and 0.24: bounced %d", m.bounced)
	}
	m.reset()
	if m.n != 0 || m.bounced != 0 || m.crossed(0) {
		t.Fatalf("reset: n %d, bounced %d", m.n, m.bounced)
	}
}

func TestTenantPauseConfig(t *testing.T) {
	for _, p := range []TenantPause{{0, 200, 60}, {1, 200, 60}, {0.04, 0, 60}, {0.04, 200, 0}} {
		cfg := testConfig()
		cfg.TenantPause = &p
		if cfg.Validate() == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
}

// pauseEnv has tenant "bad" send to mailboxes that do not exist on
// gmail-model.test and "good" to baseline.test, with a window of 4 results,
// a threshold of 0.25 and pauses of 10 minutes.
func pauseEnv(t *testing.T) *deliveryEnv {
	cfg := testConfig()
	cfg.TenantPause = &TenantPause{BounceThreshold: 0.25, Window: 4, ResumeS: 600}
	e := newDeliveryEnv(t, cfg)
	h := e.host("gmail-model.test", "mx1.gmail-model.test", "127.0.1.1")
	h.rcpt = func(to string) *Reply {
		if strings.HasPrefix(to, "nouser") {
			return &Reply{Code: 550, Enhanced: "5.1.1", Text: "The email account that you tried to reach does not exist."}
		}
		return nil
	}
	e.host("baseline.test", "mx1.baseline.test", "127.0.1.5")
	return e
}

// send ingests n messages of tenant to new mailboxes <prefix><i>: tenant
// "good" sends to baseline.test, any other to gmail-model.test.
func (e *deliveryEnv) send(tenant, prefix string, n int) []string {
	domain := "gmail-model.test"
	if tenant == "good" {
		domain = "baseline.test"
	}
	var ids []string
	for i := 0; i < n; i++ {
		ids = append(ids, e.ingest(tenant, "app@sender.test", fmt.Sprintf("%s%d@%s", prefix, len(e.events("", KindAccepted)), domain)))
	}
	return ids
}

func TestTenantPausedOnBounceRateAndResumed(t *testing.T) {
	e := pauseEnv(t)
	e.send("bad", "nouser", 2)
	e.send("bad", "user", 1)
	e.run(time.Minute)
	if n := len(e.events("", KindTenantPaused)); n != 0 {
		t.Fatalf("paused with 3 of 4 results")
	}
	e.send("bad", "user", 1) // 4th result: 2/4 bounced > 0.25
	e.run(time.Minute)
	paused := e.events("", KindTenantPaused)
	if len(paused) != 1 || paused[0].Tenant != "bad" || paused[0].MsgID != "" {
		t.Fatalf("tenant_paused events %+v", paused)
	}
	var d struct {
		Rate      float64 `json:"bounce_rate"`
		Bounced   int     `json:"bounced"`
		Window    int     `json:"window"`
		Threshold float64 `json:"threshold"`
		ResumeAt  int64   `json:"resume_at_ms"`
	}
	if err := json.Unmarshal([]byte(paused[0].Detail), &d); err != nil {
		t.Fatal(err)
	}
	if d.Rate != 0.5 || d.Bounced != 2 || d.Window != 4 || d.Threshold != 0.25 || d.ResumeAt != paused[0].AtMs+600_000 {
		t.Fatalf("tenant_paused detail %s", paused[0].Detail)
	}
	all, _ := e.st.Events()
	for i, ev := range all {
		if ev.Kind == KindTenantPaused && (i == 0 || all[i-1].Kind != KindDelivered || all[i-1].Tenant != "bad") {
			t.Fatalf("the pause must follow the crossing result at once: %+v", all[i-1])
		}
	}

	// While paused, bad's messages stay queued and good keeps delivering.
	held := e.send("bad", "user", 3)
	good := e.send("good", "user", 3)
	e.run(5 * time.Minute)
	e.allState(store.StateQueued, held...)
	e.allState(store.StateDelivered, good...)
	for _, id := range held {
		if got := e.kinds(id); got != "accepted" {
			t.Fatalf("paused tenant's message %s: %s", id, got)
		}
	}

	e.run(10 * time.Minute)
	resumed := e.events("", KindTenantResumed)
	if len(resumed) != 1 || resumed[0].Tenant != "bad" || resumed[0].AtMs != d.ResumeAt {
		t.Fatalf("tenant_resumed %+v, want at %d", resumed, d.ResumeAt)
	}
	e.allState(store.StateDelivered, held...)
	for _, id := range held {
		if at := e.events(id, KindAttemptStarted)[0].AtMs; at < d.ResumeAt {
			t.Fatalf("%s started at %d, before the resume at %d", id, at, d.ResumeAt)
		}
	}
	// The window restarted on resume: three deliveries do not fill it.
	if n := len(e.events("", KindTenantPaused)); n != 1 {
		t.Fatalf("%d pauses", n)
	}
}

func TestTenantPauseSurvivesRestart(t *testing.T) {
	e := pauseEnv(t)
	e.send("bad", "nouser", 4)
	e.run(time.Minute)
	paused := e.events("", KindTenantPaused)
	if len(paused) != 1 {
		t.Fatalf("pauses %d", len(paused))
	}
	resumeAt := paused[0].AtMs + 600_000
	e.reopen()
	held := e.send("bad", "user", 2)
	e.run(5 * time.Minute)
	e.allState(store.StateQueued, held...)
	e.run(10 * time.Minute)
	e.allState(store.StateDelivered, held...)
	if r := e.events("", KindTenantResumed); len(r) != 1 || r[0].AtMs != resumeAt {
		t.Fatalf("resumed %+v, want at %d", r, resumeAt)
	}
	// The window rebuilt from the log restarted at the resume: two
	// deliveries since then, plus two bounces, cross again.
	e.reopen()
	e.send("bad", "nouser", 2)
	e.run(time.Minute)
	if n := len(e.events("", KindTenantPaused)); n != 2 {
		t.Fatalf("%d pauses after the rebuilt window filled", n)
	}
}

func TestTenantRateLimit(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.Tenant = map[string]Limit{"bad": {Rate: &Rate{Limit: 2, PerS: 60}}}
	e := newDeliveryEnv(t, cfg)
	e.host("gmail-model.test", "mx1.gmail-model.test", "127.0.1.1")
	e.host("baseline.test", "mx1.baseline.test", "127.0.1.5")
	limited := e.send("bad", "user", 5)
	free := e.send("good", "user", 5)
	e.run(time.Hour)
	e.allState(store.StateDelivered, limited...)
	e.allState(store.StateDelivered, free...)
	for i, id := range limited {
		if at, want := e.events(id, KindAttemptStarted)[0].AtMs, int64(i/2)*60_000; at != want {
			t.Fatalf("limited message %d started at %d, want %d", i, at, want)
		}
	}
	for _, id := range free {
		if at := e.events(id, KindAttemptStarted)[0].AtMs; at != 0 {
			t.Fatalf("other tenant's message started at %d", at)
		}
	}
}
