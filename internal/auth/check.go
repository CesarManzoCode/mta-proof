package auth

import (
	"bytes"
	"context"
	"net"
	"net/mail"
	"strings"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/dkim"
)

// Results as receiver.jsonl writes them (contract 3.4).
const (
	Pass = "pass"
	Fail = "fail"
)

// Input is what the receiver knows at the end of DATA.
type Input struct {
	IP       net.IP
	Helo     string
	MailFrom string // "" for the null reverse-path
	Message  []byte
}

// Result is the outcome of the three checks. Each of SPF, DKIM and DMARC is
// Pass or Fail: everything but an SPF "pass", a valid DKIM signature or an
// aligned identifier under a discovered DMARC record is Fail.
type Result struct {
	SPF, DKIM, DMARC string
	// Policy is the effective DMARC policy of the Author Domain, after
	// t=y lowers it one level [R-9989 §4.7]; "" when no record applies or
	// a DNS error left DMARC undecided [R-9989 §5.3.6].
	Policy string
	// SPFResult is the full check_host() result [R-7208 §2.6]; DKIMDomains
	// are the d= of the valid signatures; Author is the RFC5322.From domain.
	SPFResult   string
	DKIMDomains []string
	Author      string
}

// Check runs SPF on the MAIL FROM identity (the HELO identity for a null
// reverse-path) [R-7208 §4], verifies every DKIM signature [R-6376 §6] and
// evaluates DMARC [R-9989 §5.3] with the lab resolver.
func Check(ctx context.Context, r *Resolver, in Input) Result {
	res := Result{SPF: Fail, DKIM: Fail, DMARC: Fail}
	spfRes, _ := spf.CheckHostWithSender(in.IP, in.Helo, in.MailFrom,
		spf.WithResolver(r), spf.WithContext(ctx))
	res.SPFResult = string(spfRes)
	if spfRes == spf.Pass {
		res.SPF = Pass
	}
	verifs, _ := dkim.VerifyWithOptions(bytes.NewReader(in.Message), &dkim.VerifyOptions{
		LookupTXT: func(name string) ([]string, error) { return r.LookupTXT(ctx, name) },
	})
	for _, v := range verifs {
		if v.Err == nil {
			res.DKIMDomains = append(res.DKIMDomains, strings.ToLower(v.Domain))
		}
	}
	if len(res.DKIMDomains) > 0 {
		res.DKIM = Pass
	}

	author, ok := authorDomain(in.Message)
	if !ok {
		return res // §5.3.1: no single Author Domain, DMARC cannot be evaluated
	}
	res.Author = author
	pol, err := r.Discover(ctx, author)
	if err != nil || pol.Record == nil {
		return res // §5.3.2 no record; §5.3.6 a DNS error decides nothing
	}
	var ids []struct{ id, mode string }
	// §4.4.2: only the MAIL FROM identity counts for DMARC.
	if res.SPF == Pass && in.MailFrom != "" {
		ids = append(ids, struct{ id, mode string }{domainOf(in.MailFrom), pol.Record.ASPF})
	}
	for _, d := range res.DKIMDomains {
		ids = append(ids, struct{ id, mode string }{d, pol.Record.ADKIM})
	}
	orgs := map[string]string{}
	if pol.org != "" {
		orgs[author] = pol.org
	}
	for _, id := range ids {
		ok, err := r.aligned(ctx, id.id, author, id.mode, orgs)
		if err != nil {
			return res
		}
		if ok {
			res.DMARC = Pass
			break
		}
	}
	res.Policy = pol.Effective
	if pol.Record.T == "y" {
		switch res.Policy {
		case PolicyReject:
			res.Policy = PolicyQuarantine
		case PolicyQuarantine:
			res.Policy = PolicyNone
		}
	}
	return res
}

// authorDomain extracts the domain of the single RFC5322.From address
// [R-9989 §5.3.1].
func authorDomain(msg []byte) (string, bool) {
	m, err := mail.ReadMessage(bytes.NewReader(msg))
	if err != nil || len(m.Header["From"]) != 1 {
		return "", false
	}
	list, err := mail.ParseAddressList(m.Header["From"][0])
	if err != nil || len(list) != 1 {
		return "", false
	}
	d := domainOf(list[0].Address)
	return d, d != ""
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return strings.ToLower(strings.TrimSuffix(addr[i+1:], "."))
	}
	return ""
}
