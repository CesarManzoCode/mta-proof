package engine

import (
	"encoding/json"
	"sort"

	"github.com/CesarManzoCode/mta-proof/internal/store"
)

// tenantState is the bounce monitor of one tenant (contract 2.6): the last
// window terminal results since it last resumed, as a ring, and its pause.
type tenantState struct {
	ring               []bool // true: bounced
	next, n, bounced   int
	paused             bool
	pausedAt, resumeAt int64
}

func (t *tenantState) add(bounced bool) {
	if t.n == len(t.ring) {
		if t.ring[t.next] {
			t.bounced--
		}
	} else {
		t.n++
	}
	t.ring[t.next] = bounced
	if bounced {
		t.bounced++
	}
	t.next = (t.next + 1) % len(t.ring)
}

// reset empties the window: a resumed tenant starts a new one.
func (t *tenantState) reset() {
	clear(t.ring)
	t.next, t.n, t.bounced = 0, 0, 0
}

// rate is the fraction of bounced in the window.
func (t *tenantState) rate() float64 { return float64(t.bounced) / float64(t.n) }

// crossed reports whether the window is full and its bounce rate is above
// the threshold.
func (t *tenantState) crossed(threshold float64) bool {
	return t.n == len(t.ring) && t.rate() > threshold
}

func (c *Core) tenantOf(name string) *tenantState {
	t := c.s.tenants[name]
	if t == nil {
		t = &tenantState{ring: make([]bool, c.s.cfg.TenantPause.Window)}
		c.s.tenants[name] = t
	}
	return t
}

// tenantResult feeds a terminal result of tenant to its monitor and pauses
// the tenant at once when the window crosses the threshold. Results that
// arrive while the tenant is paused (attempts already in flight) do not
// count.
func (c *Core) tenantResult(tenant string, bounced bool) {
	if c.s.cfg == nil || c.s.cfg.TenantPause == nil {
		return
	}
	p := c.s.cfg.TenantPause
	t := c.tenantOf(tenant)
	if t.paused {
		return
	}
	t.add(bounced)
	if !t.crossed(p.BounceThreshold) {
		return
	}
	now := c.nowMs()
	t.paused, t.pausedAt, t.resumeAt = true, now, now+secondsMs(p.ResumeS)
	c.writeEvent(&store.Event{Kind: KindTenantPaused, Tenant: tenant, Detail: detail(map[string]any{
		"bounce_rate": t.rate(), "bounced": t.bounced, "window": p.Window, "threshold": p.BounceThreshold,
		"resume_at_ms": t.resumeAt})}, now)
}

// tenantPaused reports whether tenant may not start attempts.
func (c *Core) tenantPaused(tenant string) bool {
	t := c.s.tenants[tenant]
	return t != nil && t.paused
}

// resumeTenants resumes, in name order, the paused tenants whose pause has
// ended, and returns the earliest end of the pauses still running (0 if
// none).
func (c *Core) resumeTenants(now int64) int64 {
	names := make([]string, 0, len(c.s.tenants))
	for name, t := range c.s.tenants {
		if t.paused {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var wake int64
	for _, name := range names {
		t := c.s.tenants[name]
		if now < t.resumeAt {
			if wake == 0 || t.resumeAt < wake {
				wake = t.resumeAt
			}
			continue
		}
		t.paused = false
		t.reset()
		c.writeEvent(&store.Event{Kind: KindTenantResumed, Tenant: name,
			Detail: detail(map[string]any{"paused_at_ms": t.pausedAt})}, now)
	}
	return wake
}

// restoreTenants replays the results and pauses of the log, in seq order,
// so a restart keeps each tenant's window and pause. Replayed results never
// pause: a crossing and its pause are committed together.
func (c *Core) restoreTenants(evs []store.TenantEvent) {
	if c.s.cfg.TenantPause == nil {
		return
	}
	for _, e := range evs {
		t := c.tenantOf(e.Tenant)
		switch e.Kind {
		case KindTenantPaused:
			var d struct {
				ResumeAtMs int64 `json:"resume_at_ms"`
			}
			json.Unmarshal([]byte(e.Detail), &d)
			t.paused, t.pausedAt, t.resumeAt = true, e.AtMs, d.ResumeAtMs
		case KindTenantResumed:
			t.paused = false
			t.reset()
		default:
			if !t.paused {
				t.add(e.Kind == KindBounced)
			}
		}
	}
}
