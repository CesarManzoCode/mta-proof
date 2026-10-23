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
	"strings"
	"testing"
	"time"

	"github.com/CesarManzoCode/mta-proof/internal/sim"
	"github.com/CesarManzoCode/mta-proof/internal/tlslab"
)

const tlsHost = "mx1.baseline.test"

// Server modes of the TLS tests: the three certificates of tlslab, a
// server that does not offer STARTTLS and one that offers it but cannot
// complete the handshake.
const (
	modeNoSTARTTLS = "no-starttls"
	modeBroken     = "broken-handshake"
)

type recvLine struct {
	Ev   string   `json:"ev"`
	Rcpt string   `json:"rcpt"`
	TLS  *sim.TLS `json:"tls"`
}

// tlsReceiver serves tlsHost on a high port of 127.0.0.1 in mode and
// returns the port and a function that stops it and returns its log.
func tlsReceiver(t *testing.T, lab *tlslab.Lab, mode string) (int, func() []recvLine) {
	t.Helper()
	dir := t.TempDir()
	profiles, err := sim.LoadProfiles("../../profiles", nil)
	if err != nil {
		t.Fatal(err)
	}
	recv, err := sim.NewReceiver(sim.Config{Dir: dir, NoSync: true, StartTLS: mode != modeNoSTARTTLS,
		Profiles: map[string]*sim.Profile{"baseline": profiles["baseline"]}})
	if err != nil {
		t.Fatal(err)
	}
	srv := sim.NewServer(recv)
	if mode != modeNoSTARTTLS && mode != modeBroken {
		if srv.TLS, err = lab.ServerConfig(mode); err != nil {
			t.Fatal(err)
		}
	}
	addr, err := srv.ListenSMTP(tlsHost, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() []recvLine {
		// Let the receiver see the end of every connection first, so a
		// handshake the client aborted is logged before the shutdown.
		for deadline := time.Now().Add(5 * time.Second); !stopped && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			open := 0
			for _, l := range readRecv(t, filepath.Join(dir, "receiver.jsonl")) {
				switch l.Ev {
				case "connect":
					open++
				case "close", "drop":
					open--
				}
			}
			if open == 0 {
				break
			}
		}
		if !stopped {
			srv.Close()
			recv.Close()
			stopped = true
		}
		return readRecv(t, filepath.Join(dir, "receiver.jsonl"))
	}
	t.Cleanup(func() { stop() })
	return addr.(*net.TCPAddr).Port, stop
}

func readRecv(t *testing.T, path string) []recvLine {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []recvLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l recvLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		out = append(out, l)
	}
	return out
}

// TestOpenConnTLSPolicy opens a connection under each policy against each
// server mode, with certificates generated in the test, and checks D16:
// opportunistic always delivers (over TLS when the handshake works, even
// with a certificate that does not verify; in plain text otherwise), and
// required delivers only with a certificate of the trusted CA for the MX
// host name.
func TestOpenConnTLSPolicy(t *testing.T) {
	lab, err := tlslab.NewLab([]string{tlsHost})
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		ok       bool
		tls      bool   // a tls_negotiated result is recorded
		up       bool   // the message goes over TLS
		verified bool   // the certificate verified
		errHas   string // substring of the recorded error
		fallback bool
	}
	cases := []struct {
		mode, policy string
		want         want
	}{
		{tlslab.Valid, TLSOpportunistic, want{ok: true, tls: true, up: true, verified: true}},
		{tlslab.Valid, TLSRequired, want{ok: true, tls: true, up: true, verified: true}},
		{tlslab.OtherCA, TLSOpportunistic, want{ok: true, tls: true, up: true, errHas: "unknown authority"}},
		{tlslab.OtherCA, TLSRequired, want{tls: true, errHas: "unknown authority"}},
		{tlslab.WrongName, TLSOpportunistic, want{ok: true, tls: true, up: true, errHas: "not " + tlsHost}},
		{tlslab.WrongName, TLSRequired, want{tls: true, errHas: "not " + tlsHost}},
		{modeNoSTARTTLS, TLSOpportunistic, want{ok: true}},
		{modeNoSTARTTLS, TLSRequired, want{tls: true, errHas: "STARTTLS not offered"}},
		{modeBroken, TLSOpportunistic, want{ok: true, tls: true, fallback: true, errHas: "EOF"}},
		{modeBroken, TLSRequired, want{tls: true, errHas: "EOF"}},
	}
	for _, tc := range cases {
		t.Run(tc.mode+"/"+tc.policy, func(t *testing.T) {
			port, stop := tlsReceiver(t, lab, tc.mode)
			spec := &OpenSpec{Src: netip.MustParseAddr("127.0.0.1"), Port: port, TLS: tc.policy,
				MX: []MXHost{{Host: tlsHost, Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}}}
			tr := &TCPTransport{ConnectTimeout: 5 * time.Second, CommandTimeout: 5 * time.Second, DataEndTimeout: 5 * time.Second}
			c, res := openConn(context.Background(), tr, spec, "engine.mtaproof.test", lab.CA.Pool())
			w := tc.want
			if res.OK != w.ok || (c != nil) != w.ok || res.Reply != nil {
				t.Fatalf("open: ok %v conn %v reply %v, want ok %v; %+v", res.OK, c != nil, res.Reply, w.ok, res)
			}
			if res.TLSFailed != (!w.ok) {
				t.Fatalf("TLSFailed %v with ok %v", res.TLSFailed, w.ok)
			}
			if (len(res.TLS) > 0) != w.tls {
				t.Fatalf("tls results %+v, want any: %v", res.TLS, w.tls)
			}
			if w.tls {
				r := res.TLS[0]
				if r.Policy != tc.policy || r.Host != tlsHost || (r.Version != "") != w.up || r.Verified != w.verified ||
					r.Fallback != w.fallback || !strings.Contains(r.Error, w.errHas) || (w.errHas == "") != (r.Error == "") {
					t.Fatalf("tls result %+v, want %+v", r, w)
				}
				if w.up && (r.Version != "TLS 1.3" || r.Cipher == "") {
					t.Fatalf("negotiated %q %q", r.Version, r.Cipher)
				}
			}
			if w.ok {
				raw := []byte("X-Mtaproof-Id: m00000001\r\nSubject: tls\r\n\r\nbody\r\n")
				if tx := transact(c, "app@sender.test", "user@baseline.test", raw); tx.Reply == nil || tx.Reply.Code != 250 {
					t.Fatalf("transaction: %+v", tx)
				}
				quit(c)
			}

			// The receiver backs the engine: every accepted message went
			// over TLS exactly when the engine negotiated it, a required
			// failure never reached MAIL, and a refused handshake left a
			// starttls line with its error.
			lines := stop()
			var dataEnds, mails, startOK, startErr int
			for _, l := range lines {
				switch l.Ev {
				case "data_end":
					dataEnds++
					if (l.TLS != nil) != w.up || (l.TLS != nil && l.TLS.Version != "TLS 1.3") {
						t.Fatalf("data_end tls %+v, want over TLS: %v", l.TLS, w.up)
					}
				case "mail":
					mails++
				case "starttls":
					if l.TLS == nil {
						t.Fatalf("starttls line without tls")
					}
					if l.TLS.Error != "" {
						startErr++
					} else {
						startOK++
					}
				}
			}
			if w.ok != (dataEnds == 1) || (!w.ok && mails > 0) {
				t.Fatalf("receiver: %d data_end, %d mail", dataEnds, mails)
			}
			if w.up != (startOK == 1) {
				t.Fatalf("receiver: %d completed handshakes, want TLS %v", startOK, w.up)
			}
			if wantErr := tc.mode != tlslab.Valid && tc.mode != modeNoSTARTTLS && (tc.policy == TLSRequired || tc.mode == modeBroken); (startErr > 0) != wantErr {
				t.Fatalf("receiver: %d failed handshakes, want some: %v", startErr, wantErr)
			}
		})
	}
}

// TestRealDeliveryTLSPolicy runs the real-mode engine against a receiver
// that presents a certificate of another CA: the domain with the
// opportunistic policy is delivered over TLS, the one with the required
// policy is deferred with subtype TLSPolicy, and both record
// tls_negotiated with the verification error.
func TestRealDeliveryTLSPolicy(t *testing.T) {
	dir := t.TempDir()
	lab, err := tlslab.NewLab([]string{tlsHost})
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(dir, "ca.pem")
	if err := lab.WriteCA(ca); err != nil {
		t.Fatal(err)
	}
	port, stop := tlsReceiver(t, lab, tlslab.OtherCA)
	dnsAddr := serveZone(t, `{"records": [
		{"name": "baseline.test", "type": "MX", "pref": 10, "host": "mx1.baseline.test"},
		{"name": "secure.test", "type": "MX", "pref": 10, "host": "mx1.baseline.test"},
		{"name": "mx1.baseline.test", "type": "A", "addr": "127.0.0.1"}
	]}`)
	cfg, err := LoadConfig("../../lab/engine.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg.HTTP, cfg.DNS, cfg.SMTPPort = "127.0.0.1:0", dnsAddr, port
	cfg.Pools = []Pool{{Name: "shared", Tenants: []string{"*"}, IPs: []PoolIP{{IP: "127.0.0.1", Warm: true}}}}
	cfg.TLS = &TLS{CAFile: ca, Domains: map[string]string{"secure.test": TLSRequired}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "engine.db")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
	const n = 3
	for i := 0; i < 2*n; i++ {
		domain := []string{"baseline.test", "secure.test"}[i%2]
		b, _ := json.Marshal(&Submission{Tenant: "acme", From: "app@sender.test", To: fmt.Sprintf("user%d@%s", i, domain),
			Subject: "TLS", Text: "hello", MsgType: TypeTransactional})
		resp, err := http.Post("http://"+api+"/v1/messages", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 202 {
			t.Fatalf("ingest %d: %d", i, resp.StatusCode)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		delivered, deferred := 0, 0
		for _, ev := range readEvents(t, db) {
			switch {
			case ev.Kind == KindDelivered:
				delivered++
			case ev.Kind == KindDeferred && ev.Subtype == "TLSPolicy":
				deferred++
			}
		}
		if delivered == n && deferred == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d delivered and %d deferred TLSPolicy after 20 s", delivered, deferred)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	negotiated := map[string]int{}
	for _, ev := range readEvents(t, db) {
		switch ev.Kind {
		case KindDelivered:
			if ev.Domain != "baseline.test" {
				t.Fatalf("delivered to %s", ev.Domain)
			}
		case KindDeferred:
			if ev.Domain != "secure.test" || ev.Class != "transient" || ev.Subtype != "TLSPolicy" || ev.RuleID != "rfc3207.tls_required" {
				t.Fatalf("deferred: %+v", ev)
			}
		case KindTLSNegotiated:
			var d struct {
				Policy   string  `json:"policy"`
				Version  *string `json:"version"`
				Verified bool    `json:"verified"`
				Error    *string `json:"error"`
			}
			if err := json.Unmarshal([]byte(ev.Detail), &d); err != nil {
				t.Fatal(err)
			}
			if d.Verified || d.Error == nil || !strings.Contains(*d.Error, "unknown authority") || ev.MXHost != tlsHost {
				t.Fatalf("tls_negotiated %+v %s", ev, ev.Detail)
			}
			if (d.Version != nil) != (d.Policy == TLSOpportunistic) {
				t.Fatalf("tls_negotiated version %v under %s", d.Version, d.Policy)
			}
			negotiated[ev.Domain]++
		}
	}
	if negotiated["baseline.test"] == 0 || negotiated["secure.test"] < n {
		t.Fatalf("tls_negotiated by domain: %v", negotiated)
	}
	for _, l := range stop() {
		if l.Ev == "data_end" && l.TLS == nil {
			t.Fatalf("data_end in plain text: %+v", l)
		}
		if l.Ev == "rcpt" && strings.HasSuffix(l.Rcpt, "@secure.test") {
			t.Fatalf("required domain reached RCPT: %+v", l)
		}
	}
}
