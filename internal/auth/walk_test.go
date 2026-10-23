package auth

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

// The example of R-9989 §4.10: eight queries for a twelve-label domain.
func TestWalkTargets(t *testing.T) {
	got := walkTargets("a.b.c.d.e.f.g.h.i.j.mail.example.com")
	want := []string{"a.b.c.d.e.f.g.h.i.j.mail.example.com", "g.h.i.j.mail.example.com",
		"h.i.j.mail.example.com", "i.j.mail.example.com", "j.mail.example.com",
		"mail.example.com", "example.com", "com"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v", got)
	}
	if got := walkTargets("a.mail.example.com"); !slices.Equal(got, []string{"a.mail.example.com", "mail.example.com", "example.com", "com"}) {
		t.Fatalf("four labels: %v", got)
	}
	if got := walkTargets("h.g.f.e.d.c.b.a"); len(got) != 8 || got[1] != "g.f.e.d.c.b.a" {
		t.Fatalf("eight labels: %v", got)
	}
}

// Policy discovery for a long Author Domain asks for at most eight DMARC
// records, and finds the record of its Organizational Domain.
func TestDiscoverQueryLimit(t *testing.T) {
	var asked []string
	r := &Resolver{Exchange: func(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
		m := new(dns.Msg)
		m.SetReply(q)
		name := q.Question[0].Name
		if strings.HasPrefix(name, "_dmarc.") {
			asked = append(asked, name)
		}
		switch {
		case name == "_dmarc.example.test." && q.Question[0].Qtype == dns.TypeTXT:
			m.Answer = append(m.Answer, &dns.TXT{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeTXT, Class: dns.ClassINET},
				Txt: []string{"v=DMARC1; p=quarantine"}})
		case strings.HasPrefix(name, "_dmarc."):
			m.Rcode = dns.RcodeNameError
		}
		return m, nil
	}}
	pol, err := r.Discover(context.Background(), "a.b.c.d.e.f.g.h.i.j.mail.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) > maxWalkQueries {
		t.Fatalf("%d DMARC queries: %v", len(asked), asked)
	}
	if pol.Record == nil || pol.Domain != "example.test" || pol.Effective != PolicyQuarantine {
		t.Fatalf("policy %+v after %v", pol, asked)
	}
}
