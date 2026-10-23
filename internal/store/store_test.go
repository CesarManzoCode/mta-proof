package store

import (
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
