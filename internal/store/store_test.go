package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "engine.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestOpenSetsWALAndFull(t *testing.T) {
	s, _ := openTemp(t)
	mode, sync, err := s.Pragmas()
	if err != nil {
		t.Fatal(err)
	}
	if mode != "wal" || sync != 2 {
		t.Fatalf("journal_mode=%s synchronous=%d, want wal and 2", mode, sync)
	}
}

// The events table has exactly the columns of contract 3.3, in order.
func TestEventsColumns(t *testing.T) {
	s, _ := openTemp(t)
	rows, err := s.DB().Query("PRAGMA table_info(events)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		got = append(got, name+" "+typ)
	}
	want := []string{"seq INTEGER", "at_ms INTEGER", "msg_id TEXT", "tenant TEXT", "kind TEXT",
		"domain TEXT", "provider TEXT", "mx_host TEXT", "src_ip TEXT", "attempt INTEGER",
		"code INTEGER", "enhanced TEXT", "text TEXT", "class TEXT", "subtype TEXT", "rule_id TEXT",
		"msg_type TEXT", "detail TEXT"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events columns\n got %v\nwant %v", got, want)
	}
}

func TestApplyIsAtomic(t *testing.T) {
	s, path := openTemp(t)
	m := &Message{N: 1, ID: "m00000001", Tenant: "t", From: "a@x.test", To: "b@y.test", Domain: "y.test",
		MsgType: "transactional", PayloadSHA256: "h", Raw: []byte("x"), State: StateQueued}
	err := s.Apply([]Write{
		{InsertMessage: m},
		{Event: &Event{Kind: "accepted", MsgID: m.ID}},
		{UpdateMessage: &Message{ID: "m99999999", State: StateQueued}}, // unknown: fails
	})
	if err == nil {
		t.Fatal("apply with an update of an unknown message succeeded")
	}
	if err := s.Apply([]Write{{InsertMessage: m}, {Event: &Event{AtMs: 5, Kind: "accepted", MsgID: m.ID, Attempt: 0}}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	snap, err := s2.Load()
	if err != nil {
		t.Fatal(err)
	}
	evs, err := s2.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Messages) != 1 || len(evs) != 1 || evs[0].Seq != 1 || evs[0].AtMs != 5 {
		t.Fatalf("after a failed and a good apply: %d messages, events %+v", len(snap.Messages), evs)
	}
	var nulls int
	if err := s2.DB().QueryRow("SELECT COUNT(*) FROM events WHERE attempt IS NULL AND code IS NULL AND tenant IS NULL").Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls != 1 {
		t.Fatal("zero-valued event fields are not stored as NULL")
	}
}

// TestMigrateAddsRetries opens a database created with the k1-02 schema,
// without the retries column and the suppressions table, and checks that
// its messages survive and gain both.
func TestMigrateAddsRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE messages (n INTEGER PRIMARY KEY, msg_id TEXT NOT NULL UNIQUE,
		tenant TEXT NOT NULL, mail_from TEXT NOT NULL, rcpt TEXT NOT NULL, domain TEXT NOT NULL,
		msg_type TEXT NOT NULL, payload_sha256 TEXT NOT NULL, raw BLOB NOT NULL, state TEXT NOT NULL,
		attempts INTEGER NOT NULL, next_at_ms INTEGER NOT NULL, accepted_at_ms INTEGER NOT NULL,
		updated_at_ms INTEGER NOT NULL);
		INSERT INTO messages VALUES (1, 'm00000001', 't', 'a@b.test', 'c@d.test', 'd.test', 'transactional',
		'x', X'00', 'queued', 2, 5, 1, 1);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m := &Message{ID: "m00000001", State: StateQueued, Attempts: 2, Retries: 3, NextAtMs: 9, UpdatedAtMs: 9}
	sp := &Suppression{Tenant: "t", Address: "c@d.test", MsgID: "m00000001", Subtype: "NoEmail", CreatedAtMs: 9}
	if err := s.Apply([]Write{{UpdateMessage: m}, {PutSuppression: sp}, {PutSuppression: sp}}); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Messages[0]; got.Retries != 3 || got.Attempts != 2 || got.To != "c@d.test" {
		t.Fatalf("migrated message: %+v", got)
	}
	if len(snap.Suppressions) != 1 || !reflect.DeepEqual(snap.Suppressions[0], sp) {
		t.Fatalf("suppressions: %+v", snap.Suppressions)
	}
}
