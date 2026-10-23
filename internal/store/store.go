// Package store is the engine's durable state: messages, idempotency keys
// and the events log (contract 2.4 and 3.3), in SQLite through
// modernc.org/sqlite in WAL mode with synchronous=FULL (D03).
//
// The store does not decide anything. The engine core produces an ordered
// list of writes and the executor hands a whole group of them to Apply,
// which commits them in one transaction (group commit).
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

// Message states. queued and inflight are live; the rest are terminal
// (contract 3.3).
const (
	StateQueued     = "queued"
	StateInflight   = "inflight"
	StateDelivered  = "delivered"
	StateBounced    = "bounced"
	StateExpired    = "expired"
	StateSuppressed = "suppressed"
)

// Terminal reports whether state is one of the terminal states of 3.3.
func Terminal(state string) bool {
	switch state {
	case StateDelivered, StateBounced, StateExpired, StateSuppressed:
		return true
	}
	return false
}

const schema = `
CREATE TABLE IF NOT EXISTS messages (
	n              INTEGER PRIMARY KEY,
	msg_id         TEXT NOT NULL UNIQUE,
	tenant         TEXT NOT NULL,
	mail_from      TEXT NOT NULL,
	rcpt           TEXT NOT NULL,
	domain         TEXT NOT NULL,
	msg_type       TEXT NOT NULL,
	payload_sha256 TEXT NOT NULL,
	raw            BLOB NOT NULL,
	state          TEXT NOT NULL,
	attempts       INTEGER NOT NULL,
	retries        INTEGER NOT NULL DEFAULT 0,
	next_at_ms     INTEGER NOT NULL,
	accepted_at_ms INTEGER NOT NULL,
	updated_at_ms  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS idempotency_keys (
	key            TEXT PRIMARY KEY,
	payload_sha256 TEXT NOT NULL,
	msg_id         TEXT NOT NULL,
	created_at_ms  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS suppressions (
	tenant        TEXT NOT NULL,
	address       TEXT NOT NULL,
	msg_id        TEXT NOT NULL,
	subtype       TEXT NOT NULL,
	created_at_ms INTEGER NOT NULL,
	PRIMARY KEY (tenant, address)
);
CREATE TABLE IF NOT EXISTS events (
	seq      INTEGER PRIMARY KEY,
	at_ms    INTEGER,
	msg_id   TEXT,
	tenant   TEXT,
	kind     TEXT,
	domain   TEXT,
	provider TEXT,
	mx_host  TEXT,
	src_ip   TEXT,
	attempt  INTEGER,
	code     INTEGER,
	enhanced TEXT,
	text     TEXT,
	class    TEXT,
	subtype  TEXT,
	rule_id  TEXT,
	msg_type TEXT,
	detail   TEXT
);
`

// Message is one row of messages: one recipient per message (D13).
type Message struct {
	N             int64
	ID            string
	Tenant        string
	From          string
	To            string
	Domain        string
	MsgType       string
	PayloadSHA256 string
	Raw           []byte
	State         string
	Attempts      int
	Retries       int // deferrals that advanced the retry schedule
	NextAtMs      int64
	AcceptedAtMs  int64
	UpdatedAtMs   int64
}

// Key is one idempotency key and the message it first created.
type Key struct {
	Key           string
	PayloadSHA256 string
	MsgID         string
	CreatedAtMs   int64
}

// Suppression is one suppressed recipient of a tenant and the message
// whose permanent failure suppressed it.
type Suppression struct {
	Tenant      string
	Address     string
	MsgID       string
	Subtype     string
	CreatedAtMs int64
}

// Event is one row of events. Seq is assigned by SQLite in write order; the
// zero value of every other field is stored as NULL.
type Event struct {
	Seq      int64
	AtMs     int64
	MsgID    string
	Tenant   string
	Kind     string
	Domain   string
	Provider string
	MXHost   string
	SrcIP    string
	Attempt  int
	Code     int
	Enhanced string
	Text     string
	Class    string
	Subtype  string
	RuleID   string
	MsgType  string
	Detail   string
}

// Write is one change produced by the core. Exactly one field is set.
type Write struct {
	InsertMessage  *Message
	UpdateMessage  *Message // only state, attempts, retries, next_at_ms and updated_at_ms are written
	PutKey         *Key
	PutSuppression *Suppression
	Event          *Event
}

// Store is an open engine.db.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path. Every connection gets WAL
// and synchronous=FULL through the DSN, so no pooled connection can commit
// with weaker durability.
func Open(path string) (*Store, error) { return open(path, 2) }

// OpenUnsynced opens the database like Open but with synchronous=OFF: a
// commit does not wait for the disk. Only the simulated executor uses it:
// no process dies in mtadst, so durability changes no output, while the
// order and content of the commits stay those of Open.
func OpenUnsynced(path string) (*Store, error) { return open(path, 0) }

func open(path string, synchronous int) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", fmt.Sprintf("synchronous(%d)", synchronous))
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// One writer: the executor serialises all writes anyway, and a single
	// connection keeps transaction order identical to Apply order.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	mode, sync, err := s.Pragmas()
	if err != nil {
		db.Close()
		return nil, err
	}
	if mode != "wal" || sync != synchronous {
		db.Close()
		return nil, fmt.Errorf("store: journal_mode=%s synchronous=%d, want wal and %d", mode, sync, synchronous)
	}
	return s, nil
}

// migrate adds the columns that later schema versions introduced to a
// database created by an earlier one.
func (s *Store) migrate() error {
	rows, err := s.db.Query("SELECT name FROM pragma_table_info('messages')")
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		cols[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if !cols["retries"] {
		if _, err := s.db.Exec("ALTER TABLE messages ADD COLUMN retries INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for read-only queries in tests and tools.
func (s *Store) DB() *sql.DB { return s.db }

// Pragmas returns journal_mode and synchronous as SQLite reports them.
func (s *Store) Pragmas() (string, int, error) {
	var mode string
	var sync int
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		return "", 0, err
	}
	if err := s.db.QueryRow("PRAGMA synchronous").Scan(&sync); err != nil {
		return "", 0, err
	}
	return mode, sync, nil
}

// Apply commits writes, in order, in a single transaction. It returns only
// after the commit is durable or has failed; on failure nothing is written.
func (s *Store) Apply(writes []Write) error {
	if len(writes) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, w := range writes {
		if err := apply(tx, w); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func apply(tx *sql.Tx, w Write) error {
	switch {
	case w.InsertMessage != nil:
		m := w.InsertMessage
		_, err := tx.Exec(`INSERT INTO messages (n, msg_id, tenant, mail_from, rcpt, domain, msg_type,
			payload_sha256, raw, state, attempts, retries, next_at_ms, accepted_at_ms, updated_at_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.N, m.ID, m.Tenant, m.From, m.To, m.Domain, m.MsgType, m.PayloadSHA256, m.Raw,
			m.State, m.Attempts, m.Retries, m.NextAtMs, m.AcceptedAtMs, m.UpdatedAtMs)
		return err
	case w.UpdateMessage != nil:
		m := w.UpdateMessage
		res, err := tx.Exec(`UPDATE messages SET state = ?, attempts = ?, retries = ?, next_at_ms = ?,
			updated_at_ms = ? WHERE msg_id = ?`, m.State, m.Attempts, m.Retries, m.NextAtMs, m.UpdatedAtMs, m.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("store: update of unknown message %s", m.ID)
		}
		return nil
	case w.PutKey != nil:
		k := w.PutKey
		_, err := tx.Exec(`INSERT OR REPLACE INTO idempotency_keys (key, payload_sha256, msg_id, created_at_ms)
			VALUES (?, ?, ?, ?)`, k.Key, k.PayloadSHA256, k.MsgID, k.CreatedAtMs)
		return err
	case w.PutSuppression != nil:
		p := w.PutSuppression
		_, err := tx.Exec(`INSERT OR IGNORE INTO suppressions (tenant, address, msg_id, subtype, created_at_ms)
			VALUES (?, ?, ?, ?, ?)`, p.Tenant, p.Address, p.MsgID, p.Subtype, p.CreatedAtMs)
		return err
	case w.Event != nil:
		e := w.Event
		_, err := tx.Exec(`INSERT INTO events (at_ms, msg_id, tenant, kind, domain, provider, mx_host, src_ip,
			attempt, code, enhanced, text, class, subtype, rule_id, msg_type, detail)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.AtMs, str(e.MsgID), str(e.Tenant), str(e.Kind), str(e.Domain), str(e.Provider),
			str(e.MXHost), str(e.SrcIP), num(e.Attempt), num(e.Code), str(e.Enhanced), str(e.Text),
			str(e.Class), str(e.Subtype), str(e.RuleID), str(e.MsgType), str(e.Detail))
		return err
	}
	return errors.New("store: empty write")
}

func str(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func num(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// Snapshot is everything the core needs to resume.
type Snapshot struct {
	Messages     []*Message     // ordered by n
	Keys         []*Key         // ordered by key
	Suppressions []*Suppression // ordered by (tenant, address)
	// Learned is the detail of every deferred or expired event that
	// recorded a learned limit, in seq order: the core rebuilds its
	// learned limits from them, so a restart does not forget them.
	Learned []string
}

// Load reads the whole live state. Messages come back in msg_id sequence
// order and keys sorted, so the core rebuilds the same state every time.
func (s *Store) Load() (*Snapshot, error) {
	snap := &Snapshot{}
	rows, err := s.db.Query(`SELECT n, msg_id, tenant, mail_from, rcpt, domain, msg_type, payload_sha256,
		raw, state, attempts, retries, next_at_ms, accepted_at_ms, updated_at_ms FROM messages ORDER BY n`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		m := &Message{}
		if err := rows.Scan(&m.N, &m.ID, &m.Tenant, &m.From, &m.To, &m.Domain, &m.MsgType,
			&m.PayloadSHA256, &m.Raw, &m.State, &m.Attempts, &m.Retries, &m.NextAtMs, &m.AcceptedAtMs,
			&m.UpdatedAtMs); err != nil {
			rows.Close()
			return nil, err
		}
		snap.Messages = append(snap.Messages, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.db.Query(`SELECT key, payload_sha256, msg_id, created_at_ms FROM idempotency_keys ORDER BY key`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		k := &Key{}
		if err := rows.Scan(&k.Key, &k.PayloadSHA256, &k.MsgID, &k.CreatedAtMs); err != nil {
			rows.Close()
			return nil, err
		}
		snap.Keys = append(snap.Keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.db.Query(`SELECT tenant, address, msg_id, subtype, created_at_ms FROM suppressions
		ORDER BY tenant, address`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		p := &Suppression{}
		if err := rows.Scan(&p.Tenant, &p.Address, &p.MsgID, &p.Subtype, &p.CreatedAtMs); err != nil {
			rows.Close()
			return nil, err
		}
		snap.Suppressions = append(snap.Suppressions, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.db.Query(`SELECT detail FROM events WHERE kind IN ('deferred', 'expired')
		AND detail LIKE '%"learned":%' ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		snap.Learned = append(snap.Learned, d)
	}
	return snap, rows.Err()
}

// Events returns the whole events table in seq order.
func (s *Store) Events() ([]Event, error) {
	rows, err := s.db.Query(`SELECT seq, at_ms, msg_id, tenant, kind, domain, provider, mx_host, src_ip,
		attempt, code, enhanced, text, class, subtype, rule_id, msg_type, detail FROM events ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var msgID, tenant, kind, domain, provider, mx, src, enh, text, class, sub, rule, mt, detail sql.NullString
		var at, attempt, code sql.NullInt64
		if err := rows.Scan(&e.Seq, &at, &msgID, &tenant, &kind, &domain, &provider, &mx, &src,
			&attempt, &code, &enh, &text, &class, &sub, &rule, &mt, &detail); err != nil {
			return nil, err
		}
		e.AtMs, e.Attempt, e.Code = at.Int64, int(attempt.Int64), int(code.Int64)
		e.MsgID, e.Tenant, e.Kind, e.Domain = msgID.String, tenant.String, kind.String, domain.String
		e.Provider, e.MXHost, e.SrcIP, e.Enhanced = provider.String, mx.String, src.String, enh.String
		e.Text, e.Class, e.Subtype, e.RuleID = text.String, class.String, sub.String, rule.String
		e.MsgType, e.Detail = mt.String, detail.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// IntegrityCheck runs PRAGMA integrity_check and returns its first line.
func (s *Store) IntegrityCheck() (string, error) {
	var res string
	err := s.db.QueryRow("PRAGMA integrity_check").Scan(&res)
	return res, err
}
