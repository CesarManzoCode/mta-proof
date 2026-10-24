// Package mtaq is the debugging tool over the engine's events log
// (contract 4.2): the causal timeline of a message and counts of events by
// tenant, domain, provider, route, source address, message type and kind.
//
// It opens engine.db read-only and shares nothing with the engine but the
// store schema, so what it prints is what the log says, not what the
// engine believes.
package mtaq

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/CesarManzoCode/mta-proof/internal/store"
)

// Dimensions are the columns agg can group by, in the order of 4.2.
var Dimensions = []string{"tenant", "domain", "provider", "mx_host", "src_ip", "msg_type", "kind"}

// Filter keeps the events whose columns equal every non-empty field.
type Filter struct {
	Tenant, Domain, Provider, SrcIP, MsgType, Kind string
}

func (f Filter) pairs() [][2]string {
	all := [][2]string{{"tenant", f.Tenant}, {"domain", f.Domain}, {"provider", f.Provider},
		{"src_ip", f.SrcIP}, {"msg_type", f.MsgType}, {"kind", f.Kind}}
	var out [][2]string
	for _, p := range all {
		if p[1] != "" {
			out = append(out, p)
		}
	}
	return out
}

// where is the SQL condition of f and its arguments; the column names are
// fixed, only the values are parameters.
func (f Filter) where() (string, []any) {
	var conds []string
	var args []any
	for _, p := range f.pairs() {
		conds = append(conds, p[0]+" = ?")
		args = append(args, p[1])
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// matches reports whether event e satisfies every condition of f.
func (f Filter) matches(e Event) bool {
	for _, p := range f.pairs() {
		if v, ok := e[p[0]].(string); !ok || v != p[1] {
			return false
		}
	}
	return true
}

// Open opens engine.db read-only: SQLite's mode=ro plus query_only, so not
// even a pragma can write. A missing file is an error, never a new database.
func Open(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	esc := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	db, err := sql.Open("sqlite", "file:"+esc+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Event is one row of events as JSON: every column of 3.3, NULL as null,
// and detail decoded.
type Event map[string]any

var intColumns = map[string]bool{"seq": true, "at_ms": true, "attempt": true, "code": true}

func readEvents(db *sql.DB, cond string, args ...any) ([]Event, error) {
	rows, err := db.Query("SELECT "+strings.Join(store.EventColumns, ", ")+" FROM events"+cond+" ORDER BY seq", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		ints := make([]sql.NullInt64, len(store.EventColumns))
		strs := make([]sql.NullString, len(store.EventColumns))
		dest := make([]any, len(store.EventColumns))
		for i, c := range store.EventColumns {
			if intColumns[c] {
				dest[i] = &ints[i]
			} else {
				dest[i] = &strs[i]
			}
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		e := Event{}
		for i, c := range store.EventColumns {
			switch {
			case intColumns[c] && ints[i].Valid:
				e[c] = ints[i].Int64
			case !intColumns[c] && strs[i].Valid:
				e[c] = strs[i].String
			default:
				e[c] = nil
			}
		}
		if d, ok := e["detail"].(string); ok {
			if !json.Valid([]byte(d)) {
				return nil, fmt.Errorf("mtaq: seq %d: detail is not JSON", e["seq"])
			}
			e["detail"] = json.RawMessage(d)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Attempt is one delivery attempt of a timeline: from its attempt_started
// to its result, with the route and address it took and what the receiver
// answered. Result is delivered, deferred, bounced or expired; interrupted
// when the engine died during it (a recovered event closed it); null while
// it is still open.
type Attempt struct {
	Attempt     *int64            `json:"attempt"`
	StartedAtMs int64             `json:"started_at_ms"`
	EndedAtMs   *int64            `json:"ended_at_ms"`
	Provider    *string           `json:"provider"`
	MXHost      *string           `json:"mx_host"`
	SrcIP       *string           `json:"src_ip"`
	Addr        *string           `json:"addr"`
	Conn        *int64            `json:"conn"`
	Reused      bool              `json:"reused"`
	TLS         []json.RawMessage `json:"tls"`
	Result      *string           `json:"result"`
	Stage       *string           `json:"stage"`
	Code        *int64            `json:"code"`
	Enhanced    *string           `json:"enhanced"`
	Text        *string           `json:"text"`
	Class       *string           `json:"class"`
	Subtype     *string           `json:"subtype"`
	RuleID      *string           `json:"rule_id"`
	Error       *string           `json:"error"`
}

// Timeline is the causal history of one message (4.2).
type Timeline struct {
	MsgID        string    `json:"msg_id"`
	Tenant       *string   `json:"tenant"`
	Domain       *string   `json:"domain"`
	MsgType      *string   `json:"msg_type"`
	AcceptedAtMs *int64    `json:"accepted_at_ms"`
	State        string    `json:"state"`
	Attempts     []Attempt `json:"attempts"`
	Events       []Event   `json:"events"`
}

var results = map[string]bool{"delivered": true, "deferred": true, "bounced": true, "expired": true}

func strp(e Event, c string) *string {
	if v, ok := e[c].(string); ok {
		return &v
	}
	return nil
}

func intp(e Event, c string) *int64 {
	if v, ok := e[c].(int64); ok {
		return &v
	}
	return nil
}

// overlay sets *dst to v when v is not null.
func overlay[T any](dst **T, v *T) {
	if v != nil {
		*dst = v
	}
}

type routeDetail struct {
	Conn   *int64  `json:"conn"`
	Addr   *string `json:"addr"`
	Reused bool    `json:"reused"`
	Stage  *string `json:"stage"`
	Error  *string `json:"error"`
}

func decodeDetail(e Event) routeDetail {
	var d routeDetail
	if raw, ok := e["detail"].(json.RawMessage); ok {
		json.Unmarshal(raw, &d) // a detail of another shape leaves the fields null
	}
	return d
}

// Build folds the events of one message, in seq order, into its timeline.
// An attempt opens at attempt_started and closes at the next result
// (delivered, deferred, bounced, expired) or recovered; results with no
// open attempt (a lifetime expiry, a DNS deferral) belong to the message.
func Build(msgID string, events []Event) Timeline {
	t := Timeline{MsgID: msgID, Attempts: []Attempt{}, Events: events}
	var cur *Attempt
	terminal := ""
	for _, e := range events {
		kind, _ := e["kind"].(string)
		at, _ := e["at_ms"].(int64)
		if t.Tenant == nil {
			t.Tenant = strp(e, "tenant")
		}
		if t.Domain == nil {
			t.Domain = strp(e, "domain")
		}
		if t.MsgType == nil {
			t.MsgType = strp(e, "msg_type")
		}
		switch {
		case kind == "accepted":
			if t.AcceptedAtMs == nil {
				t.AcceptedAtMs = &at
			}
		case kind == "attempt_started":
			if cur != nil {
				// Never written by the engine: a new attempt without a
				// result for the last one. Keep both, the old one open.
				t.Attempts = append(t.Attempts, *cur)
			}
			cur = &Attempt{Attempt: intp(e, "attempt"), StartedAtMs: at, Provider: strp(e, "provider"),
				MXHost: strp(e, "mx_host"), SrcIP: strp(e, "src_ip"), TLS: []json.RawMessage{}}
		case cur == nil:
		case kind == "tls_negotiated":
			if raw, ok := e["detail"].(json.RawMessage); ok {
				cur.TLS = append(cur.TLS, raw)
			}
		case kind == "routed":
			d := decodeDetail(e)
			overlay(&cur.Provider, strp(e, "provider"))
			overlay(&cur.MXHost, strp(e, "mx_host"))
			overlay(&cur.SrcIP, strp(e, "src_ip"))
			cur.Conn, cur.Addr, cur.Reused = d.Conn, d.Addr, d.Reused
		case kind == "classified":
			cur.Class, cur.Subtype, cur.RuleID = strp(e, "class"), strp(e, "subtype"), strp(e, "rule_id")
		case results[kind]:
			d := decodeDetail(e)
			cur.EndedAtMs, cur.Result = &at, &kind
			cur.Stage, cur.Error = d.Stage, d.Error
			if kind == "delivered" && cur.Stage == nil {
				// The engine records delivered only for a 2xx to the end of
				// DATA (contract 2.6) and does not repeat the stage.
				st := "data_end"
				cur.Stage = &st
			}
			overlay(&cur.Conn, d.Conn)
			overlay(&cur.Provider, strp(e, "provider"))
			overlay(&cur.MXHost, strp(e, "mx_host"))
			overlay(&cur.SrcIP, strp(e, "src_ip"))
			cur.Code, cur.Enhanced, cur.Text = intp(e, "code"), strp(e, "enhanced"), strp(e, "text")
			overlay(&cur.Class, strp(e, "class"))
			overlay(&cur.Subtype, strp(e, "subtype"))
			overlay(&cur.RuleID, strp(e, "rule_id"))
		case kind == "recovered":
			r := "interrupted"
			cur.EndedAtMs, cur.Result = &at, &r
		}
		if cur != nil && cur.Result != nil {
			t.Attempts = append(t.Attempts, *cur)
			cur = nil
		}
		if store.Terminal(kind) {
			terminal = kind
		}
	}
	if cur != nil {
		t.Attempts = append(t.Attempts, *cur)
	}
	switch {
	case terminal != "":
		t.State = terminal
	case cur != nil:
		t.State = store.StateInflight
	default:
		t.State = store.StateQueued
	}
	return t
}

// ErrNoMatch is returned by Timeline when the message has no events or
// none of them matches the filter.
var ErrNoMatch = errors.New("no matching events")

// TimelineOf is the timeline of one message, if any of its events matches f.
func TimelineOf(db *sql.DB, msgID string, f Filter) (Timeline, error) {
	events, err := readEvents(db, " WHERE msg_id = ?", msgID)
	if err != nil {
		return Timeline{}, err
	}
	if !anyMatch(events, f) {
		return Timeline{}, fmt.Errorf("%s: %w", msgID, ErrNoMatch)
	}
	return Build(msgID, events), nil
}

// Timelines calls fn with the timeline of every message that has an event
// matching f, in the order of each message's first event.
func Timelines(db *sql.DB, f Filter, fn func(Timeline) error) error {
	events, err := readEvents(db, " WHERE msg_id IS NOT NULL")
	if err != nil {
		return err
	}
	var order []string
	byMsg := map[string][]Event{}
	for _, e := range events {
		id := e["msg_id"].(string)
		if _, ok := byMsg[id]; !ok {
			order = append(order, id)
		}
		byMsg[id] = append(byMsg[id], e)
	}
	for _, id := range order {
		if !anyMatch(byMsg[id], f) {
			continue
		}
		if err := fn(Build(id, byMsg[id])); err != nil {
			return err
		}
	}
	return nil
}

func anyMatch(events []Event, f Filter) bool {
	for _, e := range events {
		if f.matches(e) {
			return true
		}
	}
	return false
}

// ParseDims checks a --by list: known dimensions, each at most once.
func ParseDims(by string) ([]string, error) {
	known := map[string]bool{}
	for _, d := range Dimensions {
		known[d] = true
	}
	seen := map[string]bool{}
	var dims []string
	for _, d := range strings.Split(by, ",") {
		d = strings.TrimSpace(d)
		switch {
		case !known[d]:
			return nil, fmt.Errorf("unknown dimension %q (known: %s)", d, strings.Join(Dimensions, ","))
		case seen[d]:
			return nil, fmt.Errorf("dimension %q twice", d)
		}
		seen[d] = true
		dims = append(dims, d)
	}
	return dims, nil
}

// Agg counts the events matching f grouped by dims, ordered by dims (NULL
// first, as SQLite orders). Each row has the dimensions and count.
func Agg(db *sql.DB, dims []string, f Filter) ([]map[string]any, error) {
	if len(dims) == 0 {
		return nil, errors.New("mtaq: no dimensions")
	}
	if _, err := ParseDims(strings.Join(dims, ",")); err != nil {
		return nil, err
	}
	cols := strings.Join(dims, ", ")
	cond, args := f.where()
	rows, err := db.Query("SELECT "+cols+", COUNT(*) FROM events"+cond+" GROUP BY "+cols+" ORDER BY "+cols, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]sql.NullString, len(dims))
		var n int64
		dest := make([]any, 0, len(dims)+1)
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		dest = append(dest, &n)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := map[string]any{"count": n}
		for i, d := range dims {
			if vals[i].Valid {
				row[d] = vals[i].String
			} else {
				row[d] = nil
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
