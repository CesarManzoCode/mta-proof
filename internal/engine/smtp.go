package engine

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/textproto"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Reply is one SMTP reply: code, enhanced status code when the first line
// carries one of the reply's class, and the text.
type Reply struct {
	Code     int
	Enhanced string
	Text     string
	// Keywords are the first words, upper-cased, of the lines after the
	// first: the extensions of an EHLO reply [R-5321 §4.1.1.1].
	Keywords []string
}

func (r Reply) String() string {
	if r.Enhanced != "" {
		return fmt.Sprintf("%d %s %s", r.Code, r.Enhanced, r.Text)
	}
	return fmt.Sprintf("%d %s", r.Code, r.Text)
}

// Transport opens SMTP connections. The real executor uses TCPTransport;
// tests and the simulated executor plug in their own.
type Transport interface {
	Dial(ctx context.Context, src netip.Addr, dst netip.AddrPort) (Conn, error)
}

// Conn is one SMTP connection at the level of replies. An error means the
// connection is gone without a reply.
type Conn interface {
	Greeting() (Reply, error)
	Cmd(line string) (Reply, error)
	// Data sends the message with dot-stuffing and the final "." after a
	// 354, and returns the reply to the end of data.
	Data(msg []byte) (Reply, error)
	Close() error
}

// TCPTransport dials TCP from a chosen source address.
type TCPTransport struct {
	ConnectTimeout time.Duration
	CommandTimeout time.Duration
	DataEndTimeout time.Duration
}

// Dial connects from src to dst.
func (t *TCPTransport) Dial(ctx context.Context, src netip.Addr, dst netip.AddrPort) (Conn, error) {
	d := net.Dialer{Timeout: t.ConnectTimeout, LocalAddr: &net.TCPAddr{IP: src.AsSlice()}}
	nc, err := d.DialContext(ctx, "tcp", dst.String())
	if err != nil {
		return nil, err
	}
	return &tcpConn{raw: nc, tp: textproto.NewConn(nc), cmd: t.CommandTimeout, dataEnd: t.DataEndTimeout}, nil
}

type tcpConn struct {
	raw     net.Conn // the socket: deadlines and Close, also under TLS
	tp      *textproto.Conn
	cmd     time.Duration
	dataEnd time.Duration
}

func (c *tcpConn) deadline(d time.Duration) {
	if d > 0 {
		c.raw.SetDeadline(time.Now().Add(d))
	}
}

// StartTLS runs the client side of the TLS handshake after the 220 to
// STARTTLS; from then on the conversation goes over TLS.
func (c *tcpConn) StartTLS(cfg *tls.Config) (tls.ConnectionState, error) {
	tc := tls.Client(c.raw, cfg)
	c.deadline(c.cmd)
	if err := tc.Handshake(); err != nil {
		return tls.ConnectionState{}, err
	}
	c.tp = textproto.NewConn(tc)
	return tc.ConnectionState(), nil
}

func (c *tcpConn) read() (Reply, error) { return ReadReply(&c.tp.Reader) }

// ReadReply reads one reply, single or multi-line, and splits it with
// parseReply. An error means no complete reply arrived. The in-process
// transport of mtadst reads the simulator's wire bytes with it, so both
// executors parse replies the same way.
func ReadReply(r *textproto.Reader) (Reply, error) {
	code, msg, err := r.ReadResponse(0)
	if err != nil {
		var te *textproto.Error
		if !errors.As(err, &te) {
			return Reply{}, err
		}
	}
	return parseReply(code, msg), nil
}

func (c *tcpConn) Greeting() (Reply, error) {
	c.deadline(c.cmd)
	return c.read()
}

func (c *tcpConn) Cmd(line string) (Reply, error) {
	c.deadline(c.cmd)
	if err := c.tp.PrintfLine("%s", line); err != nil {
		return Reply{}, err
	}
	return c.read()
}

func (c *tcpConn) Data(msg []byte) (Reply, error) {
	c.deadline(c.dataEnd)
	w := c.tp.DotWriter()
	if _, err := w.Write(msg); err != nil {
		return Reply{}, err
	}
	if err := w.Close(); err != nil {
		return Reply{}, err
	}
	return c.read()
}

func (c *tcpConn) Close() error { return c.raw.Close() }

var enhancedRe = regexp.MustCompile(`^([245])\.(\d{1,3})\.(\d{1,3})(?: |$)`)

// parseReply splits each line of a reply into its enhanced code and text
// and joins the texts with spaces. The enhanced code is taken from the
// first line and only when its class is the reply's.
func parseReply(code int, msg string) Reply {
	r := Reply{Code: code}
	var texts []string
	for i, line := range strings.Split(msg, "\n") {
		if m := enhancedRe.FindStringSubmatch(line); m != nil && int(m[1][0]-'0') == code/100 {
			if i == 0 {
				r.Enhanced = m[1] + "." + m[2] + "." + m[3]
			}
			line = strings.TrimPrefix(line[len(m[0]):], " ")
		}
		if f := strings.Fields(line); i > 0 && len(f) > 0 {
			r.Keywords = append(r.Keywords, strings.ToUpper(f[0]))
		}
		texts = append(texts, line)
	}
	r.Text = strings.Join(texts, " ")
	return r
}

// TryResult is one address that could not be used while opening a
// connection.
type TryResult struct {
	Host  string `json:"host"`
	Addr  string `json:"addr"`
	Error string `json:"error"`
}

// OpenResult is the outcome of opening a connection: an established
// session (OK), a receiver reply that refused the session (Reply), or no
// usable address at all.
type OpenResult struct {
	OK     bool
	MXHost string
	Addr   string
	Reply  *Reply // greeting or HELO refusal: the session never started
	Stage  string // "connect" or "ehlo" with Reply
	Tried  []TryResult
	// TLS lists the STARTTLS negotiations, one per address where the
	// policy asked for one; TLSFailed is set when a required policy
	// could not be met at some address.
	TLS       []TLSResult
	TLSFailed bool
}

// TLSResult is one STARTTLS negotiation: the tls_negotiated event.
type TLSResult struct {
	Host     string
	Addr     string
	Policy   string
	Version  string // empty without a completed handshake
	Cipher   string
	Verified bool   // the certificate chains to the trusted CA for Host
	Error    string // why the certificate did not verify or TLS failed
	// Fallback: the handshake failed under the opportunistic policy and
	// the address was dialled again to send in plain text.
	Fallback bool
}

// tlsStarter is a Conn that can run a TLS handshake after the 220 to
// STARTTLS. A Conn without it cannot negotiate TLS.
type tlsStarter interface {
	StartTLS(cfg *tls.Config) (tls.ConnectionState, error)
}

type addrStatus int

const (
	addrFailed     addrStatus = iota // no session: try the next address
	addrOK                           // session established
	addrRefused                      // the receiver refused with a reply
	addrPlainRetry                   // opportunistic TLS failed: dial again in plain text
)

// openConn tries the MX hosts in order and, within a host, its addresses
// in order, moving on when the connection fails without a reply
// [R-5321 §5.1] or a required TLS policy is not met. A reply that refuses
// the session ends the attempt: the receiver answered, and the core
// classifies the answer. roots are the CAs a certificate must chain to.
func openConn(ctx context.Context, tr Transport, spec *OpenSpec, helo string, roots *x509.CertPool) (Conn, OpenResult) {
	var res OpenResult
	for _, mx := range spec.MX {
		for _, a := range mx.Addrs {
			dst := netip.AddrPortFrom(a, uint16(spec.Port))
			c, st := openAddr(ctx, tr, spec, mx.Host, dst, helo, roots, false, &res)
			if st == addrPlainRetry {
				res.TLS[len(res.TLS)-1].Fallback = true
				c, st = openAddr(ctx, tr, spec, mx.Host, dst, helo, roots, true, &res)
			}
			switch st {
			case addrOK:
				res.OK, res.MXHost, res.Addr = true, mx.Host, dst.String()
				return c, res
			case addrRefused:
				return nil, res
			}
		}
	}
	return nil, res
}

// openAddr opens a session at one address: greeting, EHLO (HELO if EHLO
// is refused) and, by the TLS policy of spec, STARTTLS and EHLO again
// [R-3207 §4.2]. plain skips STARTTLS.
func openAddr(ctx context.Context, tr Transport, spec *OpenSpec, host string, dst netip.AddrPort, helo string,
	roots *x509.CertPool, plain bool, res *OpenResult) (Conn, addrStatus) {
	fail := func(err error) addrStatus {
		res.Tried = append(res.Tried, TryResult{Host: host, Addr: dst.String(), Error: err.Error()})
		return addrFailed
	}
	refuse := func(c Conn, rep Reply, stage string) addrStatus {
		c.Close()
		res.MXHost, res.Addr, res.Reply, res.Stage = host, dst.String(), &rep, stage
		return addrRefused
	}
	c, err := tr.Dial(ctx, spec.Src, dst)
	if err != nil {
		return nil, fail(err)
	}
	g, err := c.Greeting()
	if err != nil {
		c.Close()
		return nil, fail(err)
	}
	if g.Code != 220 {
		return nil, refuse(c, g, "connect")
	}
	esmtp := true
	rep, err := c.Cmd("EHLO " + helo)
	if err == nil && rep.Code/100 != 2 && rep.Code != 421 {
		esmtp = false
		rep, err = c.Cmd("HELO " + helo)
	}
	if err != nil {
		c.Close()
		return nil, fail(err)
	}
	if rep.Code/100 != 2 {
		return nil, refuse(c, rep, "ehlo")
	}
	offered := esmtp && slices.Contains(rep.Keywords, "STARTTLS")
	if plain || (!offered && spec.TLS != TLSRequired) {
		return c, addrOK
	}
	t := TLSResult{Host: host, Addr: dst.String(), Policy: spec.TLS}
	up, alive := false, true
	if offered {
		up, alive = startTLS(c, &t, roots)
	} else {
		t.Error = "STARTTLS not offered"
	}
	res.TLS = append(res.TLS, t)
	switch {
	case up:
		rep, err := c.Cmd("EHLO " + helo)
		if err != nil {
			c.Close()
			return nil, fail(err)
		}
		if rep.Code/100 != 2 {
			return nil, refuse(c, rep, "ehlo")
		}
		return c, addrOK
	case spec.TLS == TLSRequired:
		// R-3207 §4.1: a client that finds the security not good enough
		// ends the session with QUIT.
		if alive {
			quit(c)
		} else {
			c.Close()
		}
		res.TLSFailed = true
		return nil, fail(errors.New("tls required: " + t.Error))
	case alive:
		// STARTTLS refused (454) or no TLS in the transport: the
		// opportunistic policy goes on in plain text [R-3207 §4].
		return c, addrOK
	}
	c.Close()
	return nil, addrPlainRetry
}

// startTLS sends STARTTLS and runs the handshake under the policy of t,
// filling t. The certificate is checked against roots and the MX host
// name in both policies; only required aborts the handshake when it does
// not verify (D16). It reports whether TLS is up and whether the
// connection is still usable when it is not.
func startTLS(c Conn, t *TLSResult, roots *x509.CertPool) (up, alive bool) {
	st, ok := c.(tlsStarter)
	if !ok {
		t.Error = "the transport cannot negotiate TLS"
		return false, true
	}
	rep, err := c.Cmd("STARTTLS")
	if err != nil {
		t.Error = err.Error()
		return false, false
	}
	if rep.Code != 220 {
		t.Error = "STARTTLS refused: " + rep.String()
		return false, true
	}
	if roots == nil {
		roots = x509.NewCertPool() // never the system roots
	}
	var verifyErr error
	cfg := &tls.Config{
		ServerName: t.Host,
		MinVersion: tls.VersionTLS12,
		// The chain and the name are checked in VerifyConnection, so the
		// opportunistic policy can record the result without enforcing it.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				verifyErr = errors.New("tls: no server certificate")
			} else {
				inter := x509.NewCertPool()
				for _, ic := range cs.PeerCertificates[1:] {
					inter.AddCert(ic)
				}
				_, verifyErr = cs.PeerCertificates[0].Verify(x509.VerifyOptions{
					Roots: roots, Intermediates: inter, DNSName: t.Host})
			}
			if t.Policy == TLSRequired {
				return verifyErr
			}
			return nil
		},
	}
	cs, err := st.StartTLS(cfg)
	if err != nil {
		t.Error = err.Error()
		return false, false
	}
	t.Version, t.Cipher = tls.VersionName(cs.Version), tls.CipherSuiteName(cs.CipherSuite)
	t.Verified = verifyErr == nil
	if verifyErr != nil {
		t.Error = verifyErr.Error()
	}
	return true, true
}

// TxResult is the outcome of one mail transaction on a connection.
type TxResult struct {
	Stage  string // "mail", "rcpt", "data" or "data_end": where it ended
	Reply  *Reply // nil when the connection was lost without a reply
	Err    string // the transport error when Reply is nil
	Closed bool   // the connection cannot be used again
}

// transact runs MAIL, RCPT and DATA for one message (D13: one
// recipient). After a refused MAIL or RCPT it sends RSET so the
// connection can carry the next message; a 421 closes the connection
// [R-5321 §3.8].
func transact(c Conn, from, to string, raw []byte) TxResult {
	lost := func(stage string, err error) TxResult {
		return TxResult{Stage: stage, Err: err.Error(), Closed: true}
	}
	refused := func(stage string, rep Reply) TxResult {
		res := TxResult{Stage: stage, Reply: &rep, Closed: rep.Code == 421}
		if !res.Closed && stage != "data_end" {
			if _, err := c.Cmd("RSET"); err != nil {
				res.Closed = true
			}
		}
		return res
	}
	rep, err := c.Cmd("MAIL FROM:<" + from + ">")
	if err != nil {
		return lost("mail", err)
	}
	if rep.Code != 250 {
		return refused("mail", rep)
	}
	rep, err = c.Cmd("RCPT TO:<" + to + ">")
	if err != nil {
		return lost("rcpt", err)
	}
	if rep.Code != 250 && rep.Code != 251 {
		return refused("rcpt", rep)
	}
	rep, err = c.Cmd("DATA")
	if err != nil {
		return lost("data", err)
	}
	if rep.Code != 354 {
		return refused("data", rep)
	}
	rep, err = c.Data(raw)
	if err != nil {
		return lost("data_end", err)
	}
	if rep.Code/100 != 2 {
		return refused("data_end", rep)
	}
	return TxResult{Stage: "data_end", Reply: &rep}
}

// quit ends a session politely and closes the connection.
func quit(c Conn) {
	c.Cmd("QUIT")
	c.Close()
}
