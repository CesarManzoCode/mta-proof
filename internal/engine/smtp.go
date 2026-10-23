package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/textproto"
	"regexp"
	"strings"
	"time"
)

// Reply is one SMTP reply: code, enhanced status code when the first line
// carries one of the reply's class, and the text.
type Reply struct {
	Code     int
	Enhanced string
	Text     string
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
	return &tcpConn{nc: nc, tp: textproto.NewConn(nc), cmd: t.CommandTimeout, dataEnd: t.DataEndTimeout}, nil
}

type tcpConn struct {
	nc      net.Conn
	tp      *textproto.Conn
	cmd     time.Duration
	dataEnd time.Duration
}

func (c *tcpConn) deadline(d time.Duration) {
	if d > 0 {
		c.nc.SetDeadline(time.Now().Add(d))
	}
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

func (c *tcpConn) Close() error { return c.nc.Close() }

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
}

// openConn tries the MX hosts in order and, within a host, its addresses
// in order, moving on when the connection fails without a reply
// [R-5321 §5.1]. A reply that refuses the session ends the attempt: the
// receiver answered, and the core classifies the answer.
func openConn(ctx context.Context, tr Transport, spec *OpenSpec, helo string) (Conn, OpenResult) {
	var res OpenResult
	for _, mx := range spec.MX {
		for _, a := range mx.Addrs {
			dst := netip.AddrPortFrom(a, uint16(spec.Port))
			fail := func(err error) {
				res.Tried = append(res.Tried, TryResult{Host: mx.Host, Addr: dst.String(), Error: err.Error()})
			}
			c, err := tr.Dial(ctx, spec.Src, dst)
			if err != nil {
				fail(err)
				continue
			}
			g, err := c.Greeting()
			if err != nil {
				c.Close()
				fail(err)
				continue
			}
			res.MXHost, res.Addr = mx.Host, dst.String()
			if g.Code != 220 {
				c.Close()
				res.Reply, res.Stage = &g, "connect"
				return nil, res
			}
			rep, err := c.Cmd("EHLO " + helo)
			if err == nil && rep.Code/100 != 2 && rep.Code != 421 {
				rep, err = c.Cmd("HELO " + helo)
			}
			if err != nil {
				c.Close()
				res.MXHost, res.Addr = "", ""
				fail(err)
				continue
			}
			if rep.Code/100 != 2 {
				c.Close()
				res.Reply, res.Stage = &rep, "ehlo"
				return nil, res
			}
			res.OK = true
			return c, res
		}
	}
	return nil, res
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
