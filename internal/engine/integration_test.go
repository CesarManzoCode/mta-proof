package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CesarManzoCode/mta-proof/internal/clock"
	"github.com/CesarManzoCode/mta-proof/internal/sim"
	"github.com/CesarManzoCode/mta-proof/internal/store"
)

const resolverZone = `{"records": [
	{"name": "multi.test", "type": "MX", "pref": 20, "host": "mx2.multi.test"},
	{"name": "multi.test", "type": "MX", "pref": 10, "host": "mx1.multi.test"},
	{"name": "mx1.multi.test", "type": "A", "addr": "127.0.1.10"},
	{"name": "mx2.multi.test", "type": "A", "addr": "127.0.1.11"},
	{"name": "mx2.multi.test", "type": "A", "addr": "127.0.1.12"},
	{"name": "implicit.test", "type": "A", "addr": "127.0.1.20"},
	{"name": "dangling.test", "type": "MX", "pref": 10, "host": "nohost.dangling.test"},
	{"name": "nullmx.test", "type": "MX", "pref": 0, "host": "."},
	{"name": "sender.test", "type": "TXT", "text": "v=spf1 -all"}
]}`

func serveZone(t *testing.T, text string) string {
	t.Helper()
	z, err := sim.ParseZone([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := sim.ServeDNSZone(z, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	return srv.Addr()
}

// TestDNSResolver queries the lab DNS of internal/sim on a high port of
// 127.0.0.1, the configured address, not resolv.conf.
func TestDNSResolver(t *testing.T) {
	r := &DNSResolver{Server: serveZone(t, resolverZone), Timeout: 2 * time.Second}
	ctx := context.Background()

	res := r.Resolve(ctx, "multi.test")
	if res.Status != ResolvedOK || res.Implicit || len(res.MX) != 2 {
		t.Fatalf("multi.test: %+v", res)
	}
	ordered := orderMX(res.MX, func(int, func(int, int)) {})
	if ordered[0].Host != "mx1.multi.test" || ordered[1].Host != "mx2.multi.test" || len(ordered[1].Addrs) != 2 {
		t.Fatalf("multi.test ordered: %+v", ordered)
	}

	res = r.Resolve(ctx, "implicit.test")
	if res.Status != ResolvedOK || !res.Implicit || len(res.MX) != 1 || res.MX[0].Host != "implicit.test" ||
		res.MX[0].Pref != 0 || res.MX[0].Addrs[0].String() != "127.0.1.20" {
		t.Fatalf("implicit MX: %+v", res)
	}
	if res = r.Resolve(ctx, "nullmx.test"); res.Status != ResolvedNullMX {
		t.Fatalf("null MX: %+v", res)
	}
	if res = r.Resolve(ctx, "nowhere.test"); res.Status != ResolvedNXDomain {
		t.Fatalf("nxdomain: %+v", res)
	}
	if res = r.Resolve(ctx, "dangling.test"); res.Status != ResolvedUnusable {
		t.Fatalf("MX without address: %+v", res)
	}
	// TXT only: an empty MX answer means the implicit MX, which has no
	// address here.
	if res = r.Resolve(ctx, "sender.test"); res.Status != ResolvedUnusable || !res.Implicit {
		t.Fatalf("no MX and no A: %+v", res)
	}
	// The lab DNS refuses names outside .test: a temporary error.
	if res = r.Resolve(ctx, "example.com"); res.Status != ResolvedTemporary {
		t.Fatalf("refused: %+v", res)
	}
	// Nobody answers: a temporary error.
	dead := &DNSResolver{Server: "127.0.0.1:1", Timeout: 200 * time.Millisecond}
	if res = dead.Resolve(ctx, "multi.test"); res.Status != ResolvedTemporary {
		t.Fatalf("no server: %+v", res)
	}
}

// TestRealDeliveryAgainstSimulator runs the real-mode engine (HTTP ingest,
// DNS client, TCP transport, deliverer and group commit) against the
// simulator's DNS and SMTP receiver with the baseline profile, all on
// high ports of 127.0.0.1.
func TestRealDeliveryAgainstSimulator(t *testing.T) {
	dir := t.TempDir()
	profiles, err := sim.LoadProfiles("../../profiles", nil)
	if err != nil {
		t.Fatal(err)
	}
	recv, err := sim.NewReceiver(sim.Config{Dir: dir, Profiles: map[string]*sim.Profile{"baseline": profiles["baseline"]}})
	if err != nil {
		t.Fatal(err)
	}
	srv := sim.NewServer(recv)
	smtpAddr, err := srv.ListenSMTP("mx1.baseline.test", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.Close()
		recv.Close()
	})
	dnsAddr := serveZone(t, `{"records": [
		{"name": "baseline.test", "type": "MX", "pref": 10, "host": "mx1.baseline.test"},
		{"name": "mx1.baseline.test", "type": "A", "addr": "127.0.0.1"},
		{"name": "nullmx.test", "type": "MX", "pref": 0, "host": "."}
	]}`)

	cfg, err := LoadConfig("../../lab/engine.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg.HTTP, cfg.DNS = "127.0.0.1:0", dnsAddr
	cfg.Pools = []Pool{{Name: "shared", Tenants: []string{"*"}, IPs: []PoolIP{{IP: "127.0.0.1", Warm: true}}}}
	cfg.SMTPPort = smtpAddr.(*net.TCPAddr).Port
	cfg.Limits.Domain = map[string]Limit{"*": {Connections: 2, MsgsPerConn: 4}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "engine.db")
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunConfig{DB: db, Config: cfg, Ready: func(a net.Addr) { ready <- a.String() }})
	}()
	var api string
	select {
	case api = <-ready:
	case err := <-done:
		t.Fatalf("engine did not start: %v", err)
	}

	const n = 20
	var ids []string
	for i := 0; i < n; i++ {
		to := fmt.Sprintf("user%d@baseline.test", i)
		if i == n-1 {
			to = "nobody@nullmx.test"
		}
		b, _ := json.Marshal(&Submission{Tenant: "acme", From: "app@sender.test", To: to, Subject: "Receipt",
			Text: fmt.Sprintf("message %d\n.leading dot", i), MsgType: TypeTransactional})
		resp, err := http.Post("http://"+api+"/v1/messages", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]string
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if resp.StatusCode != 202 {
			t.Fatalf("ingest %d: %d %v", i, resp.StatusCode, out)
		}
		ids = append(ids, out["id"])
	}

	// Wait for every message to reach a terminal state.
	deadline := time.Now().Add(20 * time.Second)
	for {
		done := 0
		for _, ev := range readEvents(t, db) {
			if ev.Kind == KindDelivered || ev.Kind == KindBounced {
				done++
			}
		}
		if done == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d messages terminal after 20 s", done, n)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	evs := readEvents(t, db)
	byMsg := map[string][]string{}
	for _, ev := range evs {
		byMsg[ev.MsgID] = append(byMsg[ev.MsgID], ev.Kind)
		if ev.Kind == KindDelivered && (ev.Code != 250 || ev.MXHost != "mx1.baseline.test" || ev.SrcIP != "127.0.0.1" ||
			ev.Provider != "baseline") {
			t.Fatalf("delivered event: %+v", ev)
		}
	}
	for _, id := range ids[:n-1] {
		if got := strings.Join(byMsg[id], ","); got != "accepted,attempt_started,routed,delivered" {
			t.Fatalf("%s events %s", id, got)
		}
	}
	if got := strings.Join(byMsg[ids[n-1]], ","); got != "accepted,routed,classified,bounced" {
		t.Fatalf("null MX events %s", got)
	}

	// The receiver saw each baseline message exactly once, from the
	// configured source address, with the stored bytes.
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	conns := map[int64]int{}
	f, err := os.Open(filepath.Join(dir, "receiver.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev struct {
			Ev    string `json:"ev"`
			Conn  int64  `json:"conn"`
			Src   string `json:"src_ip"`
			MsgID string `json:"msg_id"`
			Code  int    `json:"code"`
			SHA   string `json:"sha256"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Src != "127.0.0.1" {
			t.Fatalf("receiver event from %s", ev.Src)
		}
		if ev.Ev == "data_end" {
			if ev.Code != 250 {
				t.Fatalf("data_end %+v", ev)
			}
			seen[ev.MsgID]++
			conns[ev.Conn]++
			for _, m := range snap.Messages {
				if m.ID == ev.MsgID {
					raw, err := os.ReadFile(filepath.Join(dir, "messages", ev.SHA+".eml"))
					if err != nil || !bytes.Equal(raw, m.Raw) {
						t.Fatalf("%s: received bytes differ from the stored message (%v)", m.ID, err)
					}
				}
			}
		}
	}
	for _, id := range ids[:n-1] {
		if seen[id] != 1 {
			t.Fatalf("%s received %d times", id, seen[id])
		}
	}
	if len(seen) != n-1 {
		t.Fatalf("receiver saw %d messages, want %d", len(seen), n-1)
	}
	for c, k := range conns {
		if k > 4 {
			t.Fatalf("receiver connection %d carried %d messages, limit 4", c, k)
		}
	}
	keys := make([]int64, 0, len(conns))
	for c := range conns {
		keys = append(keys, c)
	}
	slices.Sort(keys)
	if len(keys) < 5 {
		t.Fatalf("%d connections for 19 messages at 4 per connection", len(keys))
	}
}

func readEvents(t *testing.T, db string) []store.Event {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	evs, err := st.Events()
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// TestRealExecutorCommitsBeforeNetwork runs the real executor (goroutines,
// group commit) over the fake network, whose every dial and DATA checks
// the committed events in engine.db through its own connection: the
// attempt is durable before the connection opens and before the message
// is sent, and each transaction's result is durable before the next
// attempt of that message.
func TestRealExecutorCommitsBeforeNetwork(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "engine.db")
	cfg := testConfig()
	cfg.HTTP = "127.0.0.1:0"
	cfg.Retry = RetryPolicy{InitialS: 0.05, Factor: 1, MaxS: 0.05, LifetimeS: 60}
	cfg.Limits.Domain = map[string]Limit{"*": {Connections: 3, MsgsPerConn: 2}}
	net0 := &fakeNet{t: t, clk: clock.NewVirtual(time.UnixMilli(0)), hosts: map[netip.Addr]*fakeHost{}}
	res := &fakeResolver{answers: map[string]Resolution{}, lookups: map[string]int{}}
	ok := &fakeHost{name: "mx1.ok.test"}
	flaky := &fakeHost{name: "mx1.flaky.test"}
	var mu sync.Mutex
	seen := map[string]int{}
	flaky.dataEnd = func(raw []byte) *Reply {
		mu.Lock()
		defer mu.Unlock()
		id := headerValue(raw, "X-Mtaproof-Id")
		seen[id]++
		if seen[id] < 3 {
			return &Reply{Code: 451, Enhanced: "4.3.0", Text: "Requested action aborted: local error in processing"}
		}
		return nil
	}
	net0.add(ok, "127.0.1.80")
	net0.add(flaky, "127.0.1.81")
	res.answers["ok.test"] = Resolution{Status: ResolvedOK, MX: []MXHost{mx(10, "mx1.ok.test", "127.0.1.80")}}
	res.answers["flaky.test"] = Resolution{Status: ResolvedOK, MX: []MXHost{mx(10, "mx1.flaky.test", "127.0.1.81")}}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunConfig{DB: db, Config: cfg, Transport: net0, Resolver: res,
			Ready: func(a net.Addr) { ready <- a.String() }})
	}()
	api := <-ready
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	net0.mu.Lock()
	net0.st = st
	net0.mu.Unlock()

	const n = 24
	for i := 0; i < n; i++ {
		to := "u@ok.test"
		if i%3 == 0 {
			to = "u@flaky.test"
		}
		b, _ := json.Marshal(&Submission{Tenant: "acme", From: "app@sender.test", To: to, Subject: "s", Text: "t",
			MsgType: TypeTransactional})
		resp, err := http.Post("http://"+api+"/v1/messages", "application/json", bytes.NewReader(b))
		if err != nil || resp.StatusCode != 202 {
			t.Fatalf("ingest: %v %v", resp, err)
		}
		resp.Body.Close()
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		delivered := 0
		for _, ev := range readEvents(t, db) {
			if ev.Kind == KindDelivered {
				delivered++
			}
		}
		if delivered == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d delivered", delivered, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	net0.mu.Lock()
	defer net0.mu.Unlock()
	if ok.maxOpen > 3 || flaky.maxOpen > 3 {
		t.Fatalf("max open %d and %d, limit 3", ok.maxOpen, flaky.maxOpen)
	}
	for _, h := range []*fakeHost{ok, flaky} {
		for i, k := range h.perConn {
			if k > 2 {
				t.Fatalf("%s connection %d carried %d messages, limit 2", h.name, i, k)
			}
		}
	}
	if len(flaky.got) != 3*(n/3) {
		t.Fatalf("flaky host got %d transactions, want 3 per message", len(flaky.got))
	}
}
