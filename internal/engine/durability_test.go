package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/CesarManzoCode/mta-proof/internal/clock"
	"github.com/CesarManzoCode/mta-proof/internal/store"
)

const childEnv = "MTAPROOF_ENGINE_CHILD_DB"

// TestMain turns the test binary into a real-mode engine process when the
// durability test re-executes it.
func TestMain(m *testing.M) {
	if db := os.Getenv(childEnv); db != "" {
		err := Serve(context.Background(), ServeConfig{DB: db, HTTPAddr: "127.0.0.1:0", Seed: 1,
			Ready: func(a net.Addr) { fmt.Printf("ready %s\n", a) }})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type child struct {
	cmd *exec.Cmd
	url string
}

func startChild(t *testing.T, db string) *child {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), childEnv+"="+db)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "ready ") {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("child did not get ready: %q %v", line, err)
	}
	return &child{cmd: cmd, url: "http://" + strings.TrimSpace(strings.TrimPrefix(line, "ready ")) + "/v1/messages"}
}

// acked is one request that got a 202.
type acked struct {
	key, text, id string
}

func postOne(client *http.Client, url, key, text string) (string, error) {
	b, _ := json.Marshal(sub(text))
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Idempotency-Key", key)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if resp.StatusCode != 202 {
		return "", fmt.Errorf("status %d %v", resp.StatusCode, out)
	}
	return out["id"], nil
}

// TestKillDuringIngest runs the engine as a child process, floods it with
// concurrent requests and kills it with SIGKILL at a seeded instant, several
// times over the same engine.db. Every id answered with 202 must be in the
// store with exactly one accepted event, and must replay after the restart.
func TestKillDuringIngest(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns engine processes")
	}
	const seed, rounds, workers = 1, 5, 16
	rng := clock.Rand(seed, "test.kill")
	db := filepath.Join(t.TempDir(), "engine.db")
	var mu sync.Mutex
	var acks []acked
	for round := 0; round < rounds; round++ {
		c := startChild(t, db)
		delay := time.Duration(100+rng.IntN(300)) * time.Millisecond
		var wg sync.WaitGroup
		stop := make(chan struct{})
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				client := &http.Client{Timeout: 10 * time.Second}
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					key := fmt.Sprintf("r%d-w%d-%d", round, w, i)
					id, err := postOne(client, c.url, key, key)
					if err != nil {
						return // the process died under us
					}
					mu.Lock()
					acks = append(acks, acked{key, key, id})
					mu.Unlock()
				}
			}(w)
		}
		time.Sleep(delay)
		if err := c.cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		c.cmd.Wait()
		close(stop)
		wg.Wait()
		mu.Lock()
		t.Logf("round %d: SIGKILL after %v, %d acknowledged so far", round, delay, len(acks))
		mu.Unlock()
	}
	if len(acks) == 0 {
		t.Fatal("no request was acknowledged before the kills")
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := st.IntegrityCheck(); err != nil || res != "ok" {
		t.Fatalf("integrity_check: %q %v", res, err)
	}
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	evs, err := st.Events()
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	byID := map[string]*store.Message{}
	for i, m := range snap.Messages {
		if m.N != int64(i+1) {
			t.Fatalf("msg_id sequence has a gap at %d (n=%d)", i+1, m.N)
		}
		byID[m.ID] = m
	}
	accepted := map[string]int{}
	for _, e := range evs {
		if e.Kind == KindAccepted {
			accepted[e.MsgID]++
		}
	}
	lost := 0
	for _, a := range acks {
		if byID[a.id] == nil || accepted[a.id] != 1 {
			lost++
			t.Errorf("acknowledged %s (key %s): in store %v, accepted events %d", a.id, a.key, byID[a.id] != nil, accepted[a.id])
		}
	}
	for id, n := range accepted {
		if n != 1 || byID[id] == nil {
			t.Errorf("message %s has %d accepted events", id, n)
		}
	}
	t.Logf("%d acknowledged, %d committed, %d lost", len(acks), len(snap.Messages), lost)

	// The keys are as durable as the messages: every acknowledged request
	// replays to the same id after the kills.
	c := startChild(t, db)
	defer func() { c.cmd.Process.Kill(); c.cmd.Wait() }()
	client := &http.Client{Timeout: 10 * time.Second}
	step := max(1, len(acks)/200)
	for i := 0; i < len(acks); i += step {
		id, err := postOne(client, c.url, acks[i].key, acks[i].text)
		if err != nil || id != acks[i].id {
			t.Fatalf("replay of %s: %q %v, want %s", acks[i].key, id, err, acks[i].id)
		}
	}
}
