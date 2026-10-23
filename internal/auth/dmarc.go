package auth

import (
	"context"
	"errors"
	"net"
	"strings"
)

// DMARC policies [R-9989 §4.7].
const (
	PolicyNone       = "none"
	PolicyQuarantine = "quarantine"
	PolicyReject     = "reject"
)

// maxWalkQueries bounds a DNS Tree Walk [R-9989 §4.10].
const maxWalkQueries = 8

// Record is a valid DMARC Policy Record [R-9989 §4.7]; only the tags this
// lab uses are kept: p, sp, np, adkim, aspf, psd and t. Unknown tags are
// ignored.
type Record struct {
	P, SP, NP   string
	ADKIM, ASPF string // "r" or "s"
	PSD         string // "y", "n" or "u"
	T           string // "y" or "n"
}

func validPolicy(p string) bool {
	return p == PolicyNone || p == PolicyQuarantine || p == PolicyReject
}

// ParseRecord parses a TXT value as a DMARC Policy Record. It reports false
// for a record to ignore: "v" not the first tag or not exactly "DMARC1"
// [R-9989 §4.7], or a missing or invalid "p" (or an invalid "sp" or "np")
// without a "rua" tag [R-9989 §4.10.1]; with "rua" such a record acts as
// "p=none".
func ParseRecord(txt string) (*Record, bool) {
	rec := &Record{ADKIM: "r", ASPF: "r", PSD: "u", T: "n"}
	var hasP, invalid, rua bool
	for i, part := range strings.Split(txt, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if i == 0 {
			if !ok || k != "v" || v != "DMARC1" {
				return nil, false
			}
			continue
		}
		if !ok {
			continue
		}
		switch strings.ToLower(k) {
		case "p":
			hasP, rec.P = true, strings.ToLower(v)
			invalid = invalid || !validPolicy(rec.P)
		case "sp":
			rec.SP = strings.ToLower(v)
			invalid = invalid || !validPolicy(rec.SP)
		case "np":
			rec.NP = strings.ToLower(v)
			invalid = invalid || !validPolicy(rec.NP)
		case "adkim":
			if v = strings.ToLower(v); v == "r" || v == "s" {
				rec.ADKIM = v
			}
		case "aspf":
			if v = strings.ToLower(v); v == "r" || v == "s" {
				rec.ASPF = v
			}
		case "psd":
			if v = strings.ToLower(v); v == "y" || v == "n" || v == "u" {
				rec.PSD = v
			}
		case "t":
			if v = strings.ToLower(v); v == "y" || v == "n" {
				rec.T = v
			}
		case "rua":
			rua = rua || strings.Contains(v, ":")
		}
	}
	if !hasP || invalid {
		// §4.10.1: without a valid "p" (or with an invalid "sp" or "np")
		// the record acts as "p=none" only if it has a reporting URI.
		if !rua {
			return nil, false
		}
		rec.P, rec.SP, rec.NP = PolicyNone, "", ""
	}
	return rec, true
}

// found is a valid DMARC record at a domain met during a walk.
type found struct {
	domain string
	rec    *Record
}

// dmarcAt queries _dmarc.<domain> and returns its record when exactly one
// valid record is there [R-9989 §4.10 step 2]. A missing name or record is
// not an error.
func (r *Resolver) dmarcAt(ctx context.Context, domain string) (*Record, error) {
	txts, err := r.LookupTXT(ctx, "_dmarc."+domain)
	var de *net.DNSError
	if errors.As(err, &de) && de.IsNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recs []*Record
	for _, t := range txts {
		if rec, ok := ParseRecord(t); ok {
			recs = append(recs, rec)
		}
	}
	if len(recs) != 1 {
		return nil, nil
	}
	return recs[0], nil
}

// walkTargets returns the names a DNS Tree Walk from domain queries
// [R-9989 §4.10 steps 3–7]: the domain, then (with eight or more labels)
// its last seven labels or (with fewer) its parent, then each parent up to
// the last label. There are at most eight.
func walkTargets(domain string) []string {
	labels := strings.Split(strings.ToLower(strings.TrimSuffix(domain, ".")), ".")
	out := []string{strings.Join(labels, ".")}
	if len(labels) >= maxWalkQueries {
		labels = labels[len(labels)-(maxWalkQueries-1):]
	} else {
		labels = labels[1:]
	}
	for ; len(labels) > 0; labels = labels[1:] {
		out = append(out, strings.Join(labels, "."))
	}
	return out
}

// walk runs a DNS Tree Walk from targets[0] and returns the valid records
// found, longest name first. It stops at a record with psd=y or psd=n.
func (r *Resolver) walk(ctx context.Context, targets []string) ([]found, error) {
	var out []found
	for _, t := range targets {
		rec, err := r.dmarcAt(ctx, t)
		if err != nil {
			return nil, err
		}
		if rec != nil {
			out = append(out, found{t, rec})
			if rec.PSD == "y" || rec.PSD == "n" {
				break
			}
		}
	}
	return out, nil
}

// orgDomain selects the Organizational Domain of start from the records of
// its walk [R-9989 §4.10.2]: the first, longest first, with psd=n; one label
// below the first with psd=y that is not start itself; otherwise the one
// with the fewest labels; with no record, start.
func orgDomain(start string, recs []found) string {
	for _, f := range recs {
		if f.rec.PSD == "n" {
			return f.domain
		}
		if f.rec.PSD == "y" && f.domain != start {
			below := strings.TrimSuffix(start, "."+f.domain)
			return below[strings.LastIndexByte(below, '.')+1:] + "." + f.domain
		}
	}
	if len(recs) == 0 {
		return start
	}
	return recs[len(recs)-1].domain
}

// Policy is the outcome of DMARC Policy Discovery for an Author Domain.
type Policy struct {
	Domain string  // where the record was found
	Record *Record // nil: no record, DMARC does not apply
	// Effective is the Domain Owner Assessment Policy for the Author
	// Domain: p for its own record; sp or np (then p) for a subdomain.
	Effective string
	org       string
}

// Discover runs DMARC Policy Discovery [R-9989 §4.10.1]: the record at the
// Author Domain, else at its Organizational Domain, else at its PSD.
func (r *Resolver) Discover(ctx context.Context, author string) (*Policy, error) {
	author = strings.ToLower(strings.TrimSuffix(author, "."))
	targets := walkTargets(author)
	rec, err := r.dmarcAt(ctx, author)
	if err != nil {
		return nil, err
	}
	if rec != nil {
		return &Policy{Domain: author, Record: rec, Effective: rec.P}, nil
	}
	recs, err := r.walk(ctx, targets[1:])
	if err != nil {
		return nil, err
	}
	org := orgDomain(author, recs)
	var chosen *found
	for i := range recs {
		if recs[i].domain == org {
			chosen = &recs[i]
		}
	}
	if chosen == nil {
		for i := range recs {
			if recs[i].rec.PSD == "y" {
				chosen = &recs[i]
			}
		}
	}
	if chosen == nil {
		return &Policy{org: org}, nil
	}
	p := &Policy{Domain: chosen.domain, Record: chosen.rec, org: org}
	exists, err := r.exists(ctx, author)
	if err != nil {
		return nil, err
	}
	switch {
	case !exists && chosen.rec.NP != "":
		p.Effective = chosen.rec.NP
	case chosen.rec.SP != "":
		p.Effective = chosen.rec.SP
	default:
		p.Effective = chosen.rec.P
	}
	return p, nil
}

// OrgDomain returns the Organizational Domain of a domain [R-9989 §4.10.2].
func (r *Resolver) OrgDomain(ctx context.Context, domain string) (string, error) {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	recs, err := r.walk(ctx, walkTargets(domain))
	if err != nil {
		return "", err
	}
	return orgDomain(domain, recs), nil
}

// aligned reports Identifier Alignment of an Authenticated Identifier with
// the Author Domain [R-9989 §3.2.10, §4.4]: identical in strict mode, the
// same Organizational Domain in relaxed mode.
func (r *Resolver) aligned(ctx context.Context, id, author, mode string, orgs map[string]string) (bool, error) {
	id, author = strings.ToLower(id), strings.ToLower(author)
	if id == author {
		return true, nil
	}
	if mode == "s" {
		return false, nil
	}
	org := func(d string) (string, error) {
		if o, ok := orgs[d]; ok {
			return o, nil
		}
		o, err := r.OrgDomain(ctx, d)
		if err == nil {
			orgs[d] = o
		}
		return o, err
	}
	a, err := org(author)
	if err != nil {
		return false, err
	}
	b, err := org(id)
	if err != nil {
		return false, err
	}
	return a == b, nil
}
