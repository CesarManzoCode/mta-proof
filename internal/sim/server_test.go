package sim

import (
	"bufio"
	"encoding/json"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startServer serves profile p's host on a high port of 127.0.0.1 and a
// control socket.
func startServer(t *testing.T, profiles ...*Profile) (*Server, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	m := map[string]*Profile{}
	for _, p := range profiles {
		m[p.Name] = p
	}
	r, err := NewReceiver(Config{Dir: dir, Profiles: m})
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(r)
	addr, err := srv.ListenSMTP("mx.p.test", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctl := filepath.Join(dir, "control.sock")
	if _, err := srv.ServeControl(ctl); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.Close()
		r.Close()
	})
	return srv, addr.String(), ctl, dir
}

type smtpClient struct {
	t *testing.T
	c *textproto.Conn
}

func dialSMTP(t *testing.T, addr string) *smtpClient {
	t.Helper()
	c, err := textproto.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	sc := &smtpClient{t, c}
	sc.expect(220)
	return sc
}

func (sc *smtpClient) expect(code int) string {
	sc.t.Helper()
	_, msg, err := sc.c.ReadResponse(code)
	if err != nil {
		sc.t.Fatalf("want %d: %v", code, err)
	}
	return msg
}

func (sc *smtpClient) cmd(code int, line string) string {
	sc.t.Helper()
	if err := sc.c.PrintfLine("%s", line); err != nil {
		sc.t.Fatal(err)
	}
	return sc.expect(code)
}

// sendData writes message data with dot stuffing and the final ".".
func (sc *smtpClient) sendData(body string) {
	sc.t.Helper()
	w := sc.c.DotWriter()
	w.Write([]byte(body))
	if err := w.Close(); err != nil {
		sc.t.Fatal(err)
	}
}

type controlClient struct {
	t  *testing.T
	nc net.Conn
	br *bufio.Reader
}

func dialControl(t *testing.T, path string) *controlClient {
	t.Helper()
	nc, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	return &controlClient{t, nc, bufio.NewReader(nc)}
}

func (cc *controlClient) read() map[string]any {
	cc.t.Helper()
	cc.nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := cc.br.ReadBytes('\n')
	if err != nil {
		cc.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		cc.t.Fatal(err)
	}
	return m
}

func (cc *controlClient) do(req string) map[string]any {
	cc.t.Helper()
	if _, err := cc.nc.Write([]byte(req + "\n")); err != nil {
		cc.t.Fatal(err)
	}
	resp := cc.read()
	if resp["ok"] != true {
		cc.t.Fatalf("%s: %v", req, resp)
	}
	return resp
}

func TestTCPConversationLog(t *testing.T) {
	_, addr, _, dir := startServer(t, testProfile(t, 0, ""))
	c := dialSMTP(t, addr)
	if msg := c.cmd(250, "EHLO client.test"); !strings.Contains(msg, "ENHANCEDSTATUSCODES") {
		t.Fatalf("EHLO %q", msg)
	}
	c.cmd(250, "MAIL FROM:<a@sender.test>")
	c.cmd(250, "RCPT TO:<u@p.test>")
	c.cmd(354, "DATA")
	body := "X-Mtaproof-Id: m-7\r\nSubject: dots\r\n\r\n.leading dot\r\n"
	c.sendData(body)
	c.expect(250)
	c.cmd(221, "QUIT")
	// The close event is written when the session ends, before the reply
	// is sent, so the log is complete here.
	evs := readEvents(t, dir)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e["ev"].(string))
	}
	want := "connect reply ehlo reply mail reply rcpt reply reply data_end reply close"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("events\n got %s\nwant %s", got, want)
	}
	de := eventsOf(evs, EvDataEnd)[0]
	if de["msg_id"] != "m-7" || de["code"] != 250.0 || de["src_ip"] != "127.0.0.1" || de["dst_ip"] != "127.0.0.1" {
		t.Fatalf("data_end %v", de)
	}
	// The stored message is dot-unstuffed with CRLF kept.
	stored := readFile(t, filepath.Join(dir, "messages", de["sha256"].(string)+".eml"))
	if stored != body {
		t.Fatalf("stored %q, want %q", stored, body)
	}
}

func TestTCPSilentCloseAndDropMidData(t *testing.T) {
	p := testProfile(t, 0, `
		{"id": "y.max", "kind": "max_msgs_per_conn", "params": {"limit": 1},
		 "on_violation": {"action": "close_silent"}, "source": "R-YAHOO", "assumption": true},
		{"id": "d.drop", "kind": "drop_mid_data", "params": {"every_n": 2, "after_bytes": 3}, "assumption": true}`)
	_, addr, _, dir := startServer(t, p)
	c := dialSMTP(t, addr)
	c.cmd(250, "EHLO client.test")
	c.cmd(250, "MAIL FROM:<a@sender.test>")
	c.cmd(250, "RCPT TO:<u@p.test>")
	c.cmd(354, "DATA")
	c.sendData("Subject: 1\r\n\r\nx\r\n")
	c.expect(250)
	c.c.PrintfLine("MAIL FROM:<a@sender.test>")
	if _, _, err := c.c.ReadResponse(250); err == nil {
		t.Fatal("want silent close after the per-connection limit")
	}

	// Second DATA on the profile is dropped mid-data.
	c2 := dialSMTP(t, addr)
	c2.cmd(250, "EHLO client.test")
	c2.cmd(250, "MAIL FROM:<a@sender.test>")
	c2.cmd(250, "RCPT TO:<u@p.test>")
	c2.cmd(354, "DATA")
	c2.sendData("Subject: 2\r\n\r\nlonger body\r\n")
	if _, _, err := c2.c.ReadResponse(250); err == nil {
		t.Fatal("want no reply after drop_mid_data")
	}
	waitFor(t, func() bool { return len(eventsOf(readEvents(t, dir), EvDrop)) == 2 })
	drops := eventsOf(readEvents(t, dir), EvDrop)
	if drops[0]["rule"] != "y.max" || drops[1]["rule"] != "d.drop" {
		t.Fatalf("drops %v", drops)
	}
}

func TestControlSwitchCutAndHold(t *testing.T) {
	base := testProfile(t, 0, "")
	deferP := testProfile(t, 0, `{"id": "x", "kind": "respond", "params": {"stage": "data_end", "match": {},
		"response": {"code": 451, "enhanced": "4.3.0", "text": "Requested action aborted: local error in processing"}},
		"source": "R-5321", "assumption": false}`)
	deferP.Name, deferP.Hosts = "defer", nil
	_, addr, ctl, dir := startServer(t, base, deferP)
	cc := dialControl(t, ctl)
	cc.do(`{"cmd": "ping"}`)

	// hold_250: the 250 waits until release_250.
	cc.do(`{"cmd": "hold_250", "msg_id": "m-held"}`)
	c := dialSMTP(t, addr)
	c.cmd(250, "EHLO client.test")
	c.cmd(250, "MAIL FROM:<a@sender.test>")
	c.cmd(250, "RCPT TO:<u@p.test>")
	c.cmd(354, "DATA")
	c.sendData("X-Mtaproof-Id: m-held\r\n\r\nx\r\n")
	ev := cc.read()
	if ev["ev"] != "held_250" || ev["msg_id"] != "m-held" {
		t.Fatalf("hold event %v", ev)
	}
	// The receiver already logged its acceptance, durably.
	if de := eventsOf(readEvents(t, dir), EvDataEnd); len(de) != 1 || de[0]["code"] != 250.0 {
		t.Fatalf("data_end before release %v", de)
	}
	got := make(chan error, 1)
	go func() { _, _, err := c.c.ReadResponse(250); got <- err }()
	select {
	case err := <-got:
		t.Fatalf("250 sent before release: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if r := cc.do(`{"cmd": "release_250", "msg_id": "m-held"}`); r["released"] != 1.0 {
		t.Fatalf("release %v", r)
	}
	if err := <-got; err != nil {
		t.Fatalf("250 after release: %v", err)
	}

	// switch_profile applies to new connections.
	cc.do(`{"cmd": "switch_profile", "host": "mx.p.test", "profile": "defer"}`)
	c2 := dialSMTP(t, addr)
	c2.cmd(250, "EHLO client.test")
	c2.cmd(250, "MAIL FROM:<a@sender.test>")
	c2.cmd(250, "RCPT TO:<u@p.test>")
	c2.cmd(354, "DATA")
	c2.sendData("Subject: x\r\n\r\nx\r\n")
	c2.expect(451)

	// cut closes live connections and logs drop with the control rule.
	if r := cc.do(`{"cmd": "cut", "host": "mx.p.test"}`); r["cut"] != 2.0 {
		t.Fatalf("cut %v", r)
	}
	if _, err := c2.c.ReadLine(); err == nil {
		t.Fatal("connection still open after cut")
	}
	waitFor(t, func() bool { return len(eventsOf(readEvents(t, dir), EvDrop)) == 2 })
	for _, d := range eventsOf(readEvents(t, dir), EvDrop) {
		if d["rule"] != CutRule {
			t.Fatalf("drop %v", d)
		}
	}
	if eventsOf(readEvents(t, dir), EvDataEnd)[1]["profile"] != "defer" {
		t.Fatal("switched connection logged under the old profile")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
