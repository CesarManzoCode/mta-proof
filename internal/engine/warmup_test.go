package engine

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/CesarManzoCode/mta-proof/internal/clock"
	"github.com/CesarManzoCode/mta-proof/internal/store"
)

func plan(daily, hourly []int) *WarmupPlan {
	return &WarmupPlan{Daily: capsOf(daily...), Hourly: capsOf(hourly...)}
}

// warmupConfig is testConfig with one pool of the given addresses and a
// lifetime of ten days, so messages that wait for a cap do not expire.
func warmupConfig(t *testing.T, ips ...PoolIP) *Config {
	t.Helper()
	cfg := testConfig()
	cfg.Pools = []Pool{{Name: "warming", Tenants: []string{"*"}, IPs: ips}}
	cfg.Retry.LifetimeS = 10 * 86400
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// starts counts the attempt_started events by source address and by the
// day or hour bucket of their instant.
func (e *deliveryEnv) starts(bucketMs int64) map[string]map[int64]int {
	out := map[string]map[int64]int{}
	for _, ev := range e.events("", KindAttemptStarted) {
		if out[ev.SrcIP] == nil {
			out[ev.SrcIP] = map[int64]int{}
		}
		out[ev.SrcIP][ev.AtMs/bucketMs]++
	}
	return out
}

// runTo drives the stepper up to the virtual instant ms; run counts from
// the last event executed, which a cap may leave far behind.
func (e *deliveryEnv) runTo(ms int64) {
	e.t.Helper()
	if err := e.step.Run(time.UnixMilli(ms), 1_000_000); err != nil {
		e.t.Fatal(err)
	}
}

func (e *deliveryEnv) ingestN(n int, domain string) []string {
	var ids []string
	for i := 0; i < n; i++ {
		ids = append(ids, e.ingest("acme", "app@sender.test", fmt.Sprintf("user%d@%s", i, domain)))
	}
	return ids
}

func TestWarmupConfig(t *testing.T) {
	cfg := testConfig()
	if cfg.Warmup.Source != "R-RESEND-WARMUP" || len(cfg.Warmup.Plan.Daily) != 7 || *cfg.Warmup.Plan.Daily[6] != 2000 ||
		cfg.Warmup.Plan.Hourly[2] != nil || *cfg.Warmup.Plan.Hourly[3] != 50 {
		t.Fatalf("default plan: %+v", cfg.Warmup)
	}
	a := &srcIP{daily: cfg.Warmup.Plan.Daily, hourly: cfg.Warmup.Plan.Hourly, startDay: 3}
	for _, c := range []struct {
		ms            int64
		daily, hourly int
	}{
		{0, 400, 0}, {dayMs - 1, 400, 0}, {dayMs, 700, 50}, {4 * dayMs, 2000, 150}, {40 * dayMs, 2000, 150},
	} {
		if d, h := a.caps(0, c.ms); d != c.daily || h != c.hourly {
			t.Fatalf("caps at %d: %d/%d, want %d/%d", c.ms, d, h, c.daily, c.hourly)
		}
	}
	// origin_ms moves day 0 of the run to the UTC day that holds it.
	if d, _ := a.caps(10*dayMs+5, 11*dayMs); d != 700 {
		t.Fatalf("caps with origin: %d", d)
	}
	one, zero := 1, 0
	bad := map[string][]Pool{
		"no star":       {{Name: "a", Tenants: []string{"acme"}, IPs: []PoolIP{{IP: "127.0.2.1"}}}},
		"two stars":     {{Name: "a", Tenants: []string{"*"}, IPs: []PoolIP{{IP: "127.0.2.1"}}}, {Name: "b", Tenants: []string{"*"}, IPs: []PoolIP{{IP: "127.0.2.2"}}}},
		"shared ip":     {{Name: "a", Tenants: []string{"*"}, IPs: []PoolIP{{IP: "127.0.2.1"}}}, {Name: "b", Tenants: []string{"x"}, IPs: []PoolIP{{IP: "127.0.2.1"}}}},
		"warm plan":     {{Name: "a", Tenants: []string{"*"}, IPs: []PoolIP{{IP: "127.0.2.1", Warm: true, StartDay: 2}}}},
		"zero cap":      {{Name: "a", Tenants: []string{"*"}, IPs: []PoolIP{{IP: "127.0.2.1", Plan: &WarmupPlan{Daily: []*int{&one, &zero}}}}}},
		"empty plan":    {{Name: "a", Tenants: []string{"*"}, IPs: []PoolIP{{IP: "127.0.2.1", Plan: &WarmupPlan{}}}}},
		"not ipv4":      {{Name: "a", Tenants: []string{"*"}, IPs: []PoolIP{{IP: "::1"}}}},
		"negative day":  {{Name: "a", Tenants: []string{"*"}, IPs: []PoolIP{{IP: "127.0.2.1", StartDay: -1}}}},
		"tenant twice":  {{Name: "a", Tenants: []string{"*", "x"}, IPs: []PoolIP{{IP: "127.0.2.1"}}}, {Name: "b", Tenants: []string{"x"}, IPs: []PoolIP{{IP: "127.0.2.2"}}}},
		"pool without":  {{Name: "a", Tenants: []string{"*"}}},
		"no pool names": {{Tenants: []string{"*"}, IPs: []PoolIP{{IP: "127.0.2.1"}}}},
	}
	for name, pools := range bad {
		c := testConfig()
		c.Pools = pools
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := testConfig()
	c.Pools = []Pool{{Name: "a", Tenants: []string{"*"}, IPs: []PoolIP{{IP: "127.0.2.1"}}},
		{Name: "b", Tenants: []string{"globex"}, IPs: []PoolIP{{IP: "127.0.2.2", Warm: true}}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.poolOf("globex").name != "b" || c.poolOf("acme").name != "a" || c.pools[0].ips[0].startDay != 1 ||
		c.pools[1].ips[0].daily != nil {
		t.Fatalf("pools: %+v", c.pools)
	}
}

// The caps hold per hour and per day of the virtual clock: messages wait
// in the queue, the hour change frees the hourly cap and the day change
// moves to the next plan day.
func TestWarmupCapsAcrossHourAndDay(t *testing.T) {
	e := newDeliveryEnv(t, warmupConfig(t, PoolIP{IP: "127.0.2.1", Plan: plan([]int{5, 8}, []int{2, 0})}))
	e.host("baseline.test", "mx1.baseline.test", "127.0.1.5")
	ids := e.ingestN(20, "baseline.test")

	e.runTo(hourMs / 2)
	if got := len(e.events("", KindAttemptStarted)); got != 2 {
		t.Fatalf("first half hour: %d attempts, hourly cap 2", got)
	}
	e.runTo(dayMs - 1)
	byHour := e.starts(hourMs)["127.0.2.1"]
	if byHour[0] != 2 || byHour[1] != 2 || byHour[2] != 1 || len(byHour) != 3 {
		t.Fatalf("day 0 by hour: %v (hourly 2, daily 5)", byHour)
	}
	e.runTo(dayMs + hourMs)
	byDay := e.starts(dayMs)["127.0.2.1"]
	if byDay[0] != 5 || byDay[1] != 8 {
		t.Fatalf("by day: %v, want 5 then 8 (no hourly cap on plan day 2)", byDay)
	}
	queued := 0
	for _, id := range ids {
		if e.state(id) == store.StateQueued {
			queued++
		}
	}
	if queued != 7 {
		t.Fatalf("%d queued, want 7 waiting for the next day", queued)
	}
	if got := len(e.events("", KindDeferred)); got != 0 {
		t.Fatalf("a cap must not defer: %d deferred", got)
	}
}

// Selection spreads the sends of a burst in proportion to the plan, and
// the caps of every address hold.
func TestWarmupSelectionFollowsPlan(t *testing.T) {
	e := newDeliveryEnv(t, warmupConfig(t,
		PoolIP{IP: "127.0.2.1", Plan: plan([]int{10}, nil)},
		PoolIP{IP: "127.0.2.2", Plan: plan([]int{30}, nil)},
		PoolIP{IP: "127.0.2.3", Plan: plan([]int{60}, nil)}))
	e.host("baseline.test", "mx1.baseline.test", "127.0.1.5")
	e.ingestN(50, "baseline.test")
	e.run(time.Hour)
	got := e.starts(dayMs)
	if a, b, c := got["127.0.2.1"][0], got["127.0.2.2"][0], got["127.0.2.3"][0]; a != 5 || b != 15 || c != 30 {
		t.Fatalf("half the capacity: %d/%d/%d, want 5/15/30", a, b, c)
	}
	e.ingestN(80, "baseline.test")
	e.run(time.Hour)
	got = e.starts(dayMs)
	if a, b, c := got["127.0.2.1"][0], got["127.0.2.2"][0], got["127.0.2.3"][0]; a != 10 || b != 30 || c != 60 {
		t.Fatalf("over the capacity: %d/%d/%d, want the caps 10/30/60", a, b, c)
	}
}

// An attempt in flight across the hour boundary counts in both hours: the
// receiver logs its DATA in the new hour.
func TestWarmupInflightAcrossBoundary(t *testing.T) {
	cfg := warmupConfig(t, PoolIP{IP: "127.0.2.1", Plan: plan([]int{100}, []int{2})})
	c := NewCore(&store.Snapshot{}, clock.NewVirtual(time.UnixMilli(0)), clock.Rand(1, "engine.core"))
	c.SetDelivery(cfg, clock.Rand(1, "engine.mx"))
	src := netip.MustParseAddr("127.0.2.1")
	c.chargeStart("m1", src, hourMs-10)
	c.chargeStart("m2", src, hourMs-5)
	if w := c.capWait(src, hourMs-1); w != hourMs {
		t.Fatalf("full hour: wait %d, want %d", w, hourMs)
	}
	if d, h := c.used(src, hourMs); d != 2 || h != 2 {
		t.Fatalf("in flight across the hour: used %d/%d, want 2/2", d, h)
	}
	if w := c.capWait(src, hourMs); w != 2*hourMs {
		t.Fatalf("two in flight fill the new hour: wait %d", w)
	}
	c.chargeEnd("m1", netip.Addr{}, hourMs+1) // ends in the new hour: charged there
	c.chargeEnd("m2", netip.Addr{}, hourMs+2)
	if d, h := c.used(src, hourMs+3); d != 2 || h != 2 {
		t.Fatalf("after the ends: used %d/%d, want 2/2", d, h)
	}
	c.chargeStart("m3", src, 2*hourMs+1)
	c.chargeEnd("m3", netip.Addr{}, 2*hourMs+2) // same hour: no second charge
	if d, h := c.used(src, 2*hourMs+3); d != 3 || h != 1 {
		t.Fatalf("same hour: used %d/%d, want 3/1", d, h)
	}
	c.chargeStart("m4", src, dayMs-1)
	if d, h := c.used(src, dayMs); d != 1 || h != 1 {
		t.Fatalf("in flight across the day: used %d/%d, want 1/1", d, h)
	}
}

// A restart rebuilds the use of the day from the event log.
func TestWarmupSurvivesRestart(t *testing.T) {
	e := newDeliveryEnv(t, warmupConfig(t, PoolIP{IP: "127.0.2.1", Plan: plan([]int{3, 10}, nil)}))
	e.host("baseline.test", "mx1.baseline.test", "127.0.1.5")
	e.ingestN(3, "baseline.test")
	e.runTo(hourMs)
	e.reopen()
	e.ingestN(5, "baseline.test")
	e.runTo(2 * hourMs)
	if got := e.starts(dayMs)["127.0.2.1"]; got[0] != 3 {
		t.Fatalf("after a restart on the same day: %v, daily cap 3", got)
	}
	e.runTo(dayMs + hourMs)
	if got := e.starts(dayMs)["127.0.2.1"]; got[0] != 3 || got[1] != 5 {
		t.Fatalf("next day: %v", got)
	}
}

// Without a plan the pool is the round-robin of warm addresses it always
// was.
func TestWarmPoolRoundRobin(t *testing.T) {
	e := newDeliveryEnv(t, warmupConfig(t, PoolIP{IP: "127.0.2.1", Warm: true}, PoolIP{IP: "127.0.2.2", Warm: true}))
	e.host("baseline.test", "mx1.baseline.test", "127.0.1.5")
	e.ingestN(4, "baseline.test")
	e.run(time.Minute)
	var srcs []string
	for _, ev := range e.events("", KindAttemptStarted) {
		srcs = append(srcs, ev.SrcIP)
	}
	if got := strings.Join(srcs, ","); got != "127.0.2.1,127.0.2.2,127.0.2.1,127.0.2.2" {
		t.Fatalf("sources %s", got)
	}
}
