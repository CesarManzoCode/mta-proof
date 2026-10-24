// Package auth is the receiver side of outbound authentication (D07): SPF
// [R-7208] with blitiri.com.ar/go/spf, DKIM [R-6376] with
// emersion/go-msgauth and DMARC [R-9989] evaluated here, all against the lab
// DNS.
package auth

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// Exchange sends one DNS query and returns the reply.
type Exchange func(ctx context.Context, q *dns.Msg) (*dns.Msg, error)

// Resolver answers the lookups of SPF, DKIM and DMARC from the lab DNS. It
// implements spf.DNSResolver; its errors are *net.DNSError, as the SPF
// library expects (IsNotFound for NXDOMAIN and empty answers).
type Resolver struct {
	Exchange Exchange
}

// UDPExchange queries the DNS server at addr over UDP and retries over TCP
// when the reply is truncated.
func UDPExchange(addr string, timeout time.Duration) Exchange {
	udp := &dns.Client{Net: "udp", Timeout: timeout}
	tcp := &dns.Client{Net: "tcp", Timeout: timeout}
	return func(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
		// A 2048-bit DKIM key record is close to 512 octets; advertise a
		// larger UDP buffer so the client can read it.
		q.SetEdns0(4096, false)
		r, _, err := udp.ExchangeContext(ctx, q, addr)
		if err == nil && r.Truncated {
			r, _, err = tcp.ExchangeContext(ctx, q, addr)
		}
		return r, err
	}
}

// AnswerExchange answers in process with the zone's own reply builder, the
// same code the lab DNS server runs (for tests and in-memory zones).
func AnswerExchange(answer func(*dns.Msg) *dns.Msg) Exchange {
	return func(_ context.Context, q *dns.Msg) (*dns.Msg, error) { return answer(q), nil }
}

// query runs one question and maps the reply code: NXDOMAIN and an empty
// answer are "not found", anything else but NOERROR is temporary.
func (r *Resolver) query(ctx context.Context, name string, qtype uint16) ([]dns.RR, bool, error) {
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(strings.ToLower(name)), qtype)
	resp, err := r.Exchange(ctx, q)
	if err != nil {
		return nil, false, &net.DNSError{Err: err.Error(), Name: name, IsTemporary: true}
	}
	switch resp.Rcode {
	case dns.RcodeSuccess:
	case dns.RcodeNameError:
		return nil, false, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	default:
		return nil, true, &net.DNSError{Err: dns.RcodeToString[resp.Rcode], Name: name, IsTemporary: true}
	}
	var out []dns.RR
	for _, rr := range resp.Answer {
		if rr.Header().Rrtype == qtype {
			out = append(out, rr)
		}
	}
	if len(out) == 0 {
		return nil, true, &net.DNSError{Err: "no records", Name: name, IsNotFound: true}
	}
	return out, true, nil
}

// LookupTXT returns the TXT records of name, each one's character-strings
// joined in order [R-9989 §4.5].
func (r *Resolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	rrs, _, err := r.query(ctx, name, dns.TypeTXT)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rrs))
	for _, rr := range rrs {
		out = append(out, strings.Join(rr.(*dns.TXT).Txt, ""))
	}
	return out, nil
}

// LookupMX returns the MX records of name.
func (r *Resolver) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	rrs, _, err := r.query(ctx, name, dns.TypeMX)
	if err != nil {
		return nil, err
	}
	out := make([]*net.MX, 0, len(rrs))
	for _, rr := range rrs {
		mx := rr.(*dns.MX)
		out = append(out, &net.MX{Host: mx.Mx, Pref: mx.Preference})
	}
	return out, nil
}

// LookupIPAddr returns the A records of host; the lab zone has no AAAA.
func (r *Resolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	rrs, _, err := r.query(ctx, host, dns.TypeA)
	if err != nil {
		return nil, err
	}
	out := make([]net.IPAddr, 0, len(rrs))
	for _, rr := range rrs {
		out = append(out, net.IPAddr{IP: rr.(*dns.A).A})
	}
	return out, nil
}

// LookupAddr answers reverse lookups as not found: the lab zone has no PTR
// records (and R-7208 §5.5 says not to use ptr).
func (r *Resolver) LookupAddr(_ context.Context, addr string) ([]string, error) {
	return nil, &net.DNSError{Err: "no PTR records in the lab zone", Name: addr, IsNotFound: true}
}

// exists reports whether name exists in the DNS: NXDOMAIN is the only
// "does not exist" [R-9989 §3.2.13].
func (r *Resolver) exists(ctx context.Context, name string) (bool, error) {
	_, exists, err := r.query(ctx, name, dns.TypeA)
	var de *net.DNSError
	if err != nil && !(errors.As(err, &de) && de.IsNotFound) {
		return false, err
	}
	return exists, nil
}
