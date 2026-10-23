package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/CesarManzoCode/mta-proof/internal/clock"
	"github.com/CesarManzoCode/mta-proof/internal/store"
	_ "modernc.org/sqlite"
)

type apiEnv struct {
	url    string
	runner *Runner
	reader *sql.DB // a second connection: sees only committed data
}

func newAPI(t *testing.T) *apiEnv {
	t.Helper()
	path := filepath.Join(t.TempDir(), "engine.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	core, err := OpenCore(st, clock.Real{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRunner(core, st, 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	srv := httptest.NewServer(NewAPI(r, nil))
	reader, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.Close()
		cancel()
		<-done
		reader.Close()
		st.Close()
	})
	return &apiEnv{url: srv.URL + "/v1/messages", runner: r, reader: reader}
}

func (a *apiEnv) post(t *testing.T, body any, key string) (int, map[string]string, http.Header) {
	t.Helper()
	var b []byte
	if s, ok := body.(string); ok {
		b = []byte(s)
	} else {
		b, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest("POST", a.url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]string{}
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, resp.Header
}

func (a *apiEnv) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := a.reader.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAPIIngestAndIdempotency(t *testing.T) {
	a := newAPI(t)
	code, out, _ := a.post(t, sub("hi"), "k1")
	if code != 202 || out["id"] != "m00000001" {
		t.Fatalf("first post: %d %v", code, out)
	}
	// 202 is sent after the commit: another connection already sees it.
	if n := a.count(t, "SELECT COUNT(*) FROM messages WHERE msg_id = ?", out["id"]); n != 1 {
		t.Fatal("202 returned before the message was committed")
	}
	// Same payload with other JSON spacing and key order is a replay.
	same := `{"msg_type":"transactional","text":"hi","to":"user@gmail-model.test",
		"from":"app@sender.test","subject":"Your receipt","tenant":"acme"}`
	code, out, h := a.post(t, same, "k1")
	if code != 202 || out["id"] != "m00000001" || h.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %v %v", code, out, h)
	}
	code, out, _ = a.post(t, sub("other"), "k1")
	if code != 409 || out["id"] != "m00000001" {
		t.Fatalf("conflict: %d %v", code, out)
	}
	if n := a.count(t, "SELECT COUNT(*) FROM events WHERE kind = 'idempotency_conflict'"); n != 1 {
		t.Fatalf("%d idempotency_conflict events after the 409", n)
	}
	if n := a.count(t, "SELECT COUNT(*) FROM events WHERE kind = 'accepted'"); n != 1 {
		t.Fatalf("%d accepted events, want 1", n)
	}
	if code, _, _ := a.post(t, sub("hi"), strings.Repeat("k", 256)); code != 202 {
		t.Fatalf("256-character key: %d", code)
	}
}

func TestAPIRejects(t *testing.T) {
	a := newAPI(t)
	cases := []struct {
		name string
		body any
		key  string
	}{
		{"key too long", sub("x"), strings.Repeat("k", 257)},
		{"two recipients", `{"tenant":"acme","from":"a@s.test","to":["b@g.test","c@g.test"],"subject":"s","text":"","msg_type":"transactional"}`, ""},
		{"unknown field", `{"tenant":"acme","from":"a@s.test","to":"b@g.test","cc":"c@g.test","subject":"s","text":"","msg_type":"transactional"}`, ""},
		{"bad msg_type", `{"tenant":"acme","from":"a@s.test","to":"b@g.test","subject":"s","text":"","msg_type":"x"}`, ""},
		{"trailing data", `{"tenant":"acme","from":"a@s.test","to":"b@g.test","subject":"s","text":"","msg_type":"marketing"} {}`, ""},
		{"not json", `hello`, ""},
	}
	for _, c := range cases {
		if code, _, _ := a.post(t, c.body, c.key); code != 400 {
			t.Errorf("%s: %d, want 400", c.name, code)
		}
	}
	if n := a.count(t, "SELECT COUNT(*) FROM events"); n != 0 {
		t.Fatalf("rejected requests wrote %d events", n)
	}
	resp, err := http.Get(a.url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
}

// Concurrent requests share commits and every 202 is durable.
func TestAPIGroupCommit(t *testing.T) {
	a := newAPI(t)
	const workers, each = 32, 20
	var wg sync.WaitGroup
	ids := make(chan string, workers*each)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				id, err := postOne(http.DefaultClient, a.url, fmt.Sprintf("w%d-%d", w, i), fmt.Sprintf("w%d i%d", w, i))
				if err != nil {
					t.Errorf("post: %v", err)
					return
				}
				ids <- id
			}
		}(w)
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("id %s returned twice for different keys", id)
		}
		seen[id] = true
	}
	if n := a.count(t, "SELECT COUNT(*) FROM messages"); n != workers*each || len(seen) != n {
		t.Fatalf("%d messages, %d ids, want %d", n, len(seen), workers*each)
	}
	groups, ops := a.runner.Stats()
	t.Logf("%d operations in %d commits", ops, groups)
	if ops != workers*each || groups > ops {
		t.Fatalf("runner stats: %d ops in %d commits", ops, groups)
	}
}
