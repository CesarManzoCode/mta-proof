package sim

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"
)

const (
	// maxCommandLine is generous on purpose: RFC 5321 §4.5.3.1.4 sets 512
	// octets, and a longer line gets "500 Line too long" instead of a drop.
	maxCommandLine = 4096
	// maxMessage caps the data of one message (RFC 5321 §4.5.3.1.7 asks
	// for at least 64K); larger messages get 552.
	maxMessage = 64 << 20
	// serverTimeout is RFC 5321 §4.5.3.2.7.
	serverTimeout = 5 * time.Minute
	// CutRule is the rule written on drop events caused by a cut command.
	CutRule = "control.cut"
	// ShutdownRule is the rule written on drop events caused by shutdown.
	ShutdownRule = "shutdown"
)

// Server is the socket transport of a Receiver: SMTP listeners, the set of
// live connections and the 250 holds of the control socket.
type Server struct {
	R *Receiver

	mu        sync.Mutex
	listeners []net.Listener
	conns     map[*conn]struct{}
	holds     []*hold
	wg        sync.WaitGroup
	closing   bool
}

type conn struct {
	nc      net.Conn
	sess    *Session
	mu      sync.Mutex
	cutRule string
	cutCh   chan struct{}
}

// markCut records why the simulator is cutting the connection; only the
// first reason counts.
func (c *conn) markCut(rule string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cutRule == "" {
		c.cutRule = rule
		close(c.cutCh)
	}
}

func (c *conn) cutReason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cutRule
}

// hold is a request to hold the 250 of a message (contract 4.4). An empty
// msgID matches the next accepted message.
type hold struct {
	msgID   string
	owner   *controlConn
	matched bool
	release chan struct{}
}

// NewServer wraps a receiver.
func NewServer(r *Receiver) *Server {
	return &Server{R: r, conns: map[*conn]struct{}{}}
}

// ListenSMTP serves host on addr ("ip:port") until Close.
func (s *Server) ListenSMTP(host, addr string) (net.Addr, error) {
	if s.R.ProfileOf(host) == "" {
		return nil, fmt.Errorf("%w %q", ErrUnknownHost, host)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.listeners = append(s.listeners, ln)
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.serveConn(host, nc)
			}()
		}
	}()
	return ln.Addr(), nil
}

func addrOf(a net.Addr) netip.Addr {
	if ap, err := netip.ParseAddrPort(a.String()); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.Addr{}
}

func (s *Server) serveConn(host string, nc net.Conn) {
	defer nc.Close()
	sess, res, err := s.R.Open(host, addrOf(nc.RemoteAddr()), addrOf(nc.LocalAddr()))
	if err != nil {
		return
	}
	c := &conn{nc: nc, sess: sess, cutCh: make(chan struct{})}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		sess.Cut(ShutdownRule)
		return
	}
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
	}()

	br := bufio.NewReaderSize(nc, maxCommandLine)
	for {
		if !s.send(c, res) || res.Close {
			return
		}
		nc.SetReadDeadline(time.Now().Add(serverTimeout))
		line, tooLong, err := readLine(br)
		if err != nil {
			s.lost(c)
			return
		}
		if tooLong {
			res = Result{Reply: &Response{500, "5.5.6", "Line too long"}}
			continue
		}
		res = sess.Line(line)
		if res.Reply == nil || res.Reply.Code != 354 {
			continue
		}
		if !s.send(c, res) {
			return
		}
		if res.DropMidData {
			discardData(br, res.DropAfter)
			res = sess.DropData()
			continue
		}
		nc.SetReadDeadline(time.Now().Add(serverTimeout))
		msg, oversize, err := readData(br, maxMessage)
		if err != nil {
			s.lost(c)
			return
		}
		if oversize {
			res = sess.DataOversize()
			continue
		}
		res = sess.Data(msg)
		if res.Accepted && !s.waitHold(c, res.MsgID) {
			return
		}
	}
}

// send waits for the profile latency and writes the reply.
func (s *Server) send(c *conn, res Result) bool {
	if res.Reply == nil {
		return true
	}
	if res.Delay > 0 {
		time.Sleep(res.Delay)
	}
	if _, err := c.nc.Write(res.Wire()); err != nil {
		s.lost(c)
		return false
	}
	return true
}

// lost records the end of a connection the simulator did not end itself.
func (s *Server) lost(c *conn) {
	if rule := c.cutReason(); rule != "" {
		c.sess.Cut(rule)
	} else {
		c.sess.Gone()
	}
}

// readLine reads a CRLF (or LF) terminated line. A line longer than the
// buffer is consumed and reported as too long.
func readLine(br *bufio.Reader) (string, bool, error) {
	line, err := br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		for errors.Is(err, bufio.ErrBufferFull) {
			_, err = br.ReadSlice('\n')
		}
		return "", true, err
	}
	if err != nil {
		return "", false, err
	}
	return string(bytes.TrimRight(line, "\r\n")), false, nil
}

// DecodeData undoes the DATA encoding of wire, everything after the 354 up
// to and including the "." line, exactly as the socket server does; the
// in-process transport of mtadst feeds sessions through it. A message
// above the server's size cap is reported as oversize.
func DecodeData(wire []byte) ([]byte, bool, error) {
	return readData(bufio.NewReaderSize(bytes.NewReader(wire), maxCommandLine), maxMessage)
}

// readData reads message data up to the "." line and undoes the dot
// stuffing of RFC 5321 §4.5.2, keeping line endings as received.
func readData(br *bufio.Reader, max int) ([]byte, bool, error) {
	var msg bytes.Buffer
	oversize := false
	atLineStart := true
	for {
		chunk, err := br.ReadSlice('\n')
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return nil, false, err
		}
		if atLineStart && (bytes.Equal(chunk, []byte(".\r\n")) || bytes.Equal(chunk, []byte(".\n"))) {
			return msg.Bytes(), oversize, nil
		}
		if atLineStart && len(chunk) > 0 && chunk[0] == '.' {
			chunk = chunk[1:]
		}
		atLineStart = err == nil
		if msg.Len()+len(chunk) > max {
			oversize = true
			msg.Reset()
		}
		if !oversize {
			msg.Write(chunk)
		}
	}
}

// discardData reads message data until n raw bytes were consumed or the
// "." line arrives, whichever comes first.
func discardData(br *bufio.Reader, n int) {
	consumed, atLineStart := 0, true
	for consumed < n {
		chunk, err := br.ReadSlice('\n')
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return
		}
		if atLineStart && (bytes.Equal(chunk, []byte(".\r\n")) || bytes.Equal(chunk, []byte(".\n"))) {
			return
		}
		consumed += len(chunk)
		atLineStart = err == nil
	}
}

// Cut closes the live connections that match host and srcIP (empty means
// any) and returns how many it closed.
func (s *Server) Cut(host, srcIP string) int { return s.cut(host, srcIP, CutRule) }

func (s *Server) cut(host, srcIP, rule string) int {
	s.mu.Lock()
	var victims []*conn
	for c := range s.conns {
		if host != "" && c.sess.Host() != normHost(host) {
			continue
		}
		if srcIP != "" && c.sess.SrcIP().String() != srcIP {
			continue
		}
		victims = append(victims, c)
	}
	s.mu.Unlock()
	for _, c := range victims {
		c.markCut(rule)
		c.nc.Close()
	}
	return len(victims)
}

// waitHold blocks the 250 of a held message until release_250, the owner
// control connection closes or the connection is cut. It returns false if
// the reply must not be sent.
func (s *Server) waitHold(c *conn, msgID string) bool {
	s.mu.Lock()
	var h *hold
	for _, cand := range s.holds {
		if !cand.matched && (cand.msgID == "" || cand.msgID == msgID) {
			h = cand
			h.matched, h.msgID = true, msgID
			break
		}
	}
	s.mu.Unlock()
	if h == nil {
		return true
	}
	h.owner.send(map[string]any{"ev": "held_250", "msg_id": msgID, "conn": c.sess.ID()})
	select {
	case <-h.release:
	case <-c.cutCh:
	}
	s.mu.Lock()
	s.dropHold(h)
	s.mu.Unlock()
	if rule := c.cutReason(); rule != "" {
		c.sess.Cut(rule)
		return false
	}
	return true
}

func (s *Server) dropHold(h *hold) {
	for i, cand := range s.holds {
		if cand == h {
			s.holds = append(s.holds[:i], s.holds[i+1:]...)
			return
		}
	}
}

func (s *Server) addHold(owner *controlConn, msgID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holds = append(s.holds, &hold{msgID: msgID, owner: owner, release: make(chan struct{})})
}

// release frees the held replies of owner (nil: any owner) whose message
// is msgID (empty: any) and returns how many it freed. With all, holds not
// yet matched are dropped too.
func (s *Server) release(owner *controlConn, msgID string, all bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	keep := s.holds[:0]
	for _, h := range s.holds {
		mine := owner == nil || h.owner == owner
		switch {
		case mine && h.matched && (msgID == "" || h.msgID == msgID):
			close(h.release)
			n++
		case mine && all && !h.matched:
		default:
			keep = append(keep, h)
		}
	}
	s.holds = keep
	return n
}

// Close stops the listeners, cuts every connection and releases holds.
func (s *Server) Close() {
	s.mu.Lock()
	s.closing = true
	for _, ln := range s.listeners {
		ln.Close()
	}
	s.mu.Unlock()
	s.cut("", "", ShutdownRule)
	s.release(nil, "", true)
	s.wg.Wait()
}

// controlConn is one client of the control socket.
type controlConn struct {
	mu sync.Mutex
	nc net.Conn
}

func (cc *controlConn) send(v any) {
	b, _ := json.Marshal(v)
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.nc.Write(append(b, '\n'))
}

// ControlRequest is one line of the control protocol.
type ControlRequest struct {
	Cmd     string `json:"cmd"`
	Host    string `json:"host,omitempty"`
	From    string `json:"from,omitempty"`
	Profile string `json:"profile,omitempty"`
	SrcIP   string `json:"src_ip,omitempty"`
	MsgID   string `json:"msg_id,omitempty"`
}

// ServeControl serves the control socket at path until the listener is
// closed by Close of the returned listener.
func (s *Server) ServeControl(path string) (net.Listener, error) {
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.listeners = append(s.listeners, ln)
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.serveControl(nc)
			}()
		}
	}()
	return ln, nil
}

func (s *Server) serveControl(nc net.Conn) {
	cc := &controlConn{nc: nc}
	defer func() {
		nc.Close()
		// A control client that goes away must not leave replies held.
		s.release(cc, "", true)
	}()
	sc := bufio.NewScanner(nc)
	for sc.Scan() {
		var req ControlRequest
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			cc.send(map[string]any{"ok": false, "error": "bad request: " + err.Error()})
			continue
		}
		cc.send(s.control(cc, req))
	}
}

func (s *Server) control(cc *controlConn, req ControlRequest) map[string]any {
	fail := func(err error) map[string]any { return map[string]any{"ok": false, "error": err.Error()} }
	switch req.Cmd {
	case "ping":
		return map[string]any{"ok": true}
	case "switch_profile":
		if req.Host != "" {
			if err := s.R.SwitchProfile(req.Host, req.Profile); err != nil {
				return fail(err)
			}
			return map[string]any{"ok": true, "hosts": 1}
		}
		if req.From == "" {
			return fail(errors.New("switch_profile needs host or from"))
		}
		n, err := s.R.SwitchFrom(req.From, req.Profile)
		if err != nil {
			return fail(err)
		}
		return map[string]any{"ok": true, "hosts": n}
	case "cut":
		return map[string]any{"ok": true, "cut": s.Cut(req.Host, req.SrcIP)}
	case "hold_250":
		s.addHold(cc, req.MsgID)
		return map[string]any{"ok": true}
	case "release_250":
		return map[string]any{"ok": true, "released": s.release(cc, req.MsgID, false)}
	}
	return fail(fmt.Errorf("unknown cmd %q", req.Cmd))
}
