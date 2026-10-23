// Package engine is the outbound delivery engine. Core is the single-threaded
// state machine of contract 2.2; the executors own it and persist what it
// produces.
package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/CesarManzoCode/mta-proof/internal/clock"
	"github.com/CesarManzoCode/mta-proof/internal/store"
)

// Event kinds (contract 3.3).
const (
	KindAccepted            = "accepted"
	KindIdempotentReplay    = "idempotent_replay"
	KindIdempotencyConflict = "idempotency_conflict"
	KindRouted              = "routed"
	KindAttemptStarted      = "attempt_started"
	KindResponse            = "response"
	KindClassified          = "classified"
	KindDeferred            = "deferred"
	KindDelivered           = "delivered"
	KindBounced             = "bounced"
	KindExpired             = "expired"
	KindSuppressed          = "suppressed"
	KindDSNEnqueued         = "dsn_enqueued"
	KindTenantPaused        = "tenant_paused"
	KindTenantResumed       = "tenant_resumed"
	KindTLSNegotiated       = "tls_negotiated"
	KindRecovered           = "recovered"
)

// Idempotency parameters [R-RESEND-IDEMP].
const (
	MaxIdempotencyKey = 256
	IdempotencyWindow = 24 * time.Hour
)

// ErrNotFound and ErrBadState are returned by attempt transitions.
var (
	ErrNotFound = errors.New("engine: unknown message")
	ErrBadState = errors.New("engine: transition not allowed in this state")
)

// Core is the engine state machine. It is not safe for concurrent use: one
// executor goroutine calls it. It reads time only from the injected Clock
// and randomness only from the injected generator, and every change it makes
// is appended to an ordered list of writes that the executor commits with
// TakeWrites and store.Apply before acknowledging anything.
type Core struct {
	clk    clock.Clock
	rng    *rand.Rand
	msgs   map[string]*store.Message
	keys   map[string]*store.Key
	lastN  int64
	writes []store.Write
}

// NewCore rebuilds the core from a snapshot and runs start-up recovery:
// every attempt that was started and has no recorded result goes back to
// the queue with a recovered event. Call TakeWrites and commit them before
// serving requests.
func NewCore(snap *store.Snapshot, clk clock.Clock, rng *rand.Rand) *Core {
	c := &Core{clk: clk, rng: rng, msgs: map[string]*store.Message{}, keys: map[string]*store.Key{}}
	for _, m := range snap.Messages {
		c.msgs[m.ID] = m
		if m.N > c.lastN {
			c.lastN = m.N
		}
	}
	for _, k := range snap.Keys {
		c.keys[k.Key] = k
	}
	c.recover(snap.Messages)
	return c
}

func (c *Core) nowMs() int64 { return clock.Ms(c.clk.Now()) }

func (c *Core) recover(ordered []*store.Message) {
	now := c.nowMs()
	for _, m := range ordered { // snapshot order is msg_id sequence order
		if m.State != store.StateInflight {
			continue
		}
		m.State, m.NextAtMs, m.UpdatedAtMs = store.StateQueued, now, now
		c.update(m)
		c.event(&store.Event{Kind: KindRecovered, Attempt: m.Attempts,
			Detail: detail(map[string]any{"from_state": store.StateInflight})}, m)
	}
}

// TakeWrites returns the writes produced since the last call, in order.
func (c *Core) TakeWrites() []store.Write {
	w := c.writes
	c.writes = nil
	return w
}

// Message returns the message with msg_id id, or nil. The caller must not
// modify it.
func (c *Core) Message(id string) *store.Message { return c.msgs[id] }

// IngestResult is the outcome of one ingest request.
type IngestResult struct {
	Status int    // 202 or 409
	MsgID  string // the new or replayed message; for 409, the key's message
	Replay bool
}

// Ingest admits a validated submission. With an idempotency key seen less
// than IdempotencyWindow ago, the same payload replays the original msg_id
// and another payload is a conflict; an older key is forgotten and the
// request creates a new message.
func (c *Core) Ingest(s *Submission, key string) IngestResult {
	now := c.nowMs()
	sum := s.PayloadSHA256()
	ev := &store.Event{Tenant: s.Tenant, Domain: domainOf(s.To), MsgType: s.MsgType}
	if key != "" {
		if k := c.keys[key]; k != nil && now-k.CreatedAtMs < IdempotencyWindow.Milliseconds() {
			ev.MsgID = k.MsgID
			ev.Detail = detail(map[string]any{"idempotency_key": key, "payload_sha256": sum})
			if k.PayloadSHA256 == sum {
				ev.Kind = KindIdempotentReplay
				c.writeEvent(ev, now)
				return IngestResult{Status: 202, MsgID: k.MsgID, Replay: true}
			}
			ev.Kind = KindIdempotencyConflict
			c.writeEvent(ev, now)
			return IngestResult{Status: 409, MsgID: k.MsgID}
		}
	}
	c.lastN++
	id := fmt.Sprintf("m%08d", c.lastN)
	m := &store.Message{
		N: c.lastN, ID: id, Tenant: s.Tenant, From: s.From, To: s.To, Domain: domainOf(s.To),
		MsgType: s.MsgType, PayloadSHA256: sum, Raw: BuildMessage(id, s, c.clk.Now()),
		State: store.StateQueued, NextAtMs: now, AcceptedAtMs: now, UpdatedAtMs: now,
	}
	c.msgs[id] = m
	ins := *m
	c.writes = append(c.writes, store.Write{InsertMessage: &ins})
	d := map[string]any{"payload_sha256": sum}
	if key != "" {
		k := &store.Key{Key: key, PayloadSHA256: sum, MsgID: id, CreatedAtMs: now}
		c.keys[key] = k
		c.writes = append(c.writes, store.Write{PutKey: k})
		d["idempotency_key"] = key
	}
	ev.Kind, ev.MsgID, ev.Detail = KindAccepted, id, detail(d)
	c.writeEvent(ev, now)
	return IngestResult{Status: 202, MsgID: id}
}

// Due returns up to limit queued messages whose next attempt time has
// come, ordered by (next_at_ms, msg_id sequence).
func (c *Core) Due(limit int) []*store.Message {
	now := c.nowMs()
	var due []*store.Message
	for _, m := range c.msgs {
		if m.State == store.StateQueued && m.NextAtMs <= now {
			due = append(due, m)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].NextAtMs != due[j].NextAtMs {
			return due[i].NextAtMs < due[j].NextAtMs
		}
		return due[i].N < due[j].N
	})
	if limit > 0 && len(due) > limit {
		due = due[:limit]
	}
	return due
}

// Route is where an attempt goes; its fields fill the route columns of the
// attempt's events.
type Route struct {
	Provider string
	MXHost   string
	SrcIP    string
}

// StartAttempt moves a queued message to inflight and records
// attempt_started. The executor must commit this before it opens the SMTP
// transaction, so a crash leaves a trace that recovery can find. It returns
// the attempt number, from 1.
func (c *Core) StartAttempt(id string, r Route) (int, error) {
	m := c.msgs[id]
	if m == nil {
		return 0, ErrNotFound
	}
	if m.State != store.StateQueued {
		return 0, ErrBadState
	}
	now := c.nowMs()
	m.Attempts++
	m.State, m.UpdatedAtMs = store.StateInflight, now
	c.update(m)
	c.event(&store.Event{Kind: KindAttemptStarted, Provider: r.Provider, MXHost: r.MXHost,
		SrcIP: r.SrcIP, Attempt: m.Attempts}, m)
	return m.Attempts, nil
}

// Outcome is the recorded result of an inflight attempt.
type Outcome struct {
	Kind     string // KindDelivered, KindDeferred, KindBounced or KindExpired
	Route    Route
	Code     int
	Enhanced string
	Text     string
	Class    string
	Subtype  string
	RuleID   string
	NextAtMs int64 // KindDeferred only: when the message is due again
	Detail   map[string]any
}

// FinishAttempt records the result of the current attempt of an inflight
// message. Response parsing, classification and backoff decide the Outcome
// elsewhere; the core only applies it.
func (c *Core) FinishAttempt(id string, o Outcome) error {
	m := c.msgs[id]
	if m == nil {
		return ErrNotFound
	}
	if m.State != store.StateInflight {
		return ErrBadState
	}
	now := c.nowMs()
	switch o.Kind {
	case KindDelivered:
		m.State = store.StateDelivered
	case KindBounced:
		m.State = store.StateBounced
	case KindExpired:
		m.State = store.StateExpired
	case KindDeferred:
		m.State, m.NextAtMs = store.StateQueued, o.NextAtMs
	default:
		return fmt.Errorf("engine: outcome kind %q", o.Kind)
	}
	m.UpdatedAtMs = now
	c.update(m)
	c.event(&store.Event{Kind: o.Kind, Provider: o.Route.Provider, MXHost: o.Route.MXHost,
		SrcIP: o.Route.SrcIP, Attempt: m.Attempts, Code: o.Code, Enhanced: o.Enhanced, Text: o.Text,
		Class: o.Class, Subtype: o.Subtype, RuleID: o.RuleID, Detail: detail(o.Detail)}, m)
	return nil
}

func (c *Core) update(m *store.Message) {
	cp := *m
	c.writes = append(c.writes, store.Write{UpdateMessage: &cp})
}

// event fills the message columns of e and appends it.
func (c *Core) event(e *store.Event, m *store.Message) {
	e.MsgID, e.Tenant, e.Domain, e.MsgType = m.ID, m.Tenant, m.Domain, m.MsgType
	c.writeEvent(e, c.nowMs())
}

func (c *Core) writeEvent(e *store.Event, at int64) {
	e.AtMs = at
	c.writes = append(c.writes, store.Write{Event: e})
}

// detail encodes an event detail; encoding/json sorts map keys, so the text
// is deterministic. A nil or empty map is NULL.
func detail(d map[string]any) string {
	if len(d) == 0 {
		return ""
	}
	b, err := json.Marshal(d)
	if err != nil {
		panic(err)
	}
	return string(b)
}
