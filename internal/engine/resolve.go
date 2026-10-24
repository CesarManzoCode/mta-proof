package engine

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// Resolution outcomes.
const (
	ResolvedOK        = "ok"        // MX hosts (or the implicit MX) with addresses
	ResolvedNullMX    = "null_mx"   // a single MX "." of preference 0 [R-7505 §3]
	ResolvedNXDomain  = "nxdomain"  // the domain does not exist [R-5321 §5.1]
	ResolvedUnusable  = "unusable"  // MX present but no host has an address [R-5321 §5.1]
	ResolvedTemporary = "temporary" // temporary DNS error: queue and retry [R-5321 §5.1]
)

// MXHost is one mail exchanger and its addresses, in the order the
// resolver returned them.
type MXHost struct {
	Pref  uint16       `json:"pref"`
	Host  string       `json:"host"`
	Addrs []netip.Addr `json:"addrs"`
}

// Resolution is the raw answer for one domain. The core orders it: the
// resolver does not sort or randomise, so the order decision stays in the
// deterministic core.
type Resolution struct {
	Status   string
	MX       []MXHost // ResolvedOK: hosts with at least one address
	Implicit bool     // no MX records: the domain itself, preference 0
	Err      string   // detail for the event log
}

// Resolver looks up the route of a destination domain.
type Resolver interface {
	Resolve(ctx context.Context, domain string) Resolution
}

// DNSResolver queries one DNS server directly (contract 2.3: the engine
// does not read resolv.conf). UDP first, TCP when the answer is truncated.
type DNSResolver struct {
	Server  string // "ip:port"
	Timeout time.Duration
	// Exchange, when set, answers queries in process instead of the
	// network: mtadst passes the lab zone, so both executors share the
	// interpretation of the answers.
	Exchange func(ctx context.Context, m *dns.Msg) (*dns.Msg, error)
}

var errNX = errors.New("nxdomain")

func (r *DNSResolver) query(ctx context.Context, name string, qtype uint16) ([]dns.RR, error) {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	m.RecursionDesired = true
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	var in *dns.Msg
	var err error
	if r.Exchange != nil {
		in, err = r.Exchange(ctx, m)
	} else {
		c := &dns.Client{Net: "udp", Timeout: timeout}
		in, _, err = c.ExchangeContext(ctx, m, r.Server)
		if err == nil && in.Truncated {
			c.Net = "tcp"
			in, _, err = c.ExchangeContext(ctx, m, r.Server)
		}
	}
	if err != nil {
		return nil, err
	}
	switch in.Rcode {
	case dns.RcodeSuccess:
		return in.Answer, nil
	case dns.RcodeNameError:
		return nil, errNX
	default:
		return nil, fmt.Errorf("rcode %s", dns.RcodeToString[in.Rcode])
	}
}

// Resolve implements R-5321 §5.1 and R-7505: MX lookup; NXDOMAIN is
// permanent; any other failure is temporary; no MX records means the
// implicit MX; a lone "." of preference 0 is a null MX; hosts without an
// address are unusable, and if none is usable the domain is unusable
// (unless a lookup failed temporarily). CNAMEs are not followed: the lab
// zone has none (3.9).
func (r *DNSResolver) Resolve(ctx context.Context, domain string) Resolution {
	answer, err := r.query(ctx, domain, dns.TypeMX)
	if errors.Is(err, errNX) {
		return Resolution{Status: ResolvedNXDomain, Err: "MX " + domain + ": NXDOMAIN"}
	}
	if err != nil {
		return Resolution{Status: ResolvedTemporary, Err: "MX " + domain + ": " + err.Error()}
	}
	var mx []MXHost
	for _, rr := range answer {
		if v, ok := rr.(*dns.MX); ok {
			mx = append(mx, MXHost{Pref: v.Preference, Host: v.Mx})
		}
	}
	res := Resolution{Status: ResolvedOK}
	if len(mx) == 1 && mx[0].Pref == 0 && mx[0].Host == "." {
		return Resolution{Status: ResolvedNullMX}
	}
	if len(mx) == 0 {
		res.Implicit = true
		mx = []MXHost{{Pref: 0, Host: domain}}
	}
	var temp []string
	for _, h := range mx {
		if h.Host == "." {
			continue // "." beside other MX records is not a usable host
		}
		addrs, err := r.addrs(ctx, h.Host)
		if err != nil && !errors.Is(err, errNX) {
			temp = append(temp, "A "+h.Host+": "+err.Error())
			continue
		}
		if len(addrs) > 0 {
			h.Host = strings.TrimSuffix(h.Host, ".")
			h.Addrs = addrs
			res.MX = append(res.MX, h)
		}
	}
	if len(res.MX) == 0 {
		if len(temp) > 0 {
			return Resolution{Status: ResolvedTemporary, Err: strings.Join(temp, "; ")}
		}
		return Resolution{Status: ResolvedUnusable, Implicit: res.Implicit, Err: "no MX host of " + domain + " has an address"}
	}
	return res
}

func (r *DNSResolver) addrs(ctx context.Context, host string) ([]netip.Addr, error) {
	answer, err := r.query(ctx, host, dns.TypeA)
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, rr := range answer {
		if v, ok := rr.(*dns.A); ok {
			if a, ok := netip.AddrFromSlice(v.A.To4()); ok {
				out = append(out, a)
			}
		}
	}
	return out, nil
}

// orderMX sorts hosts by preference, lower first, and shuffles hosts of
// equal preference with shuffle [R-5321 §5.1]. The input is first sorted
// by (preference, host) so the result depends only on the records and the
// random stream, not on the order of the DNS answer. Addresses of one host
// keep the resolver's order.
func orderMX(in []MXHost, shuffle func(n int, swap func(i, j int))) []MXHost {
	out := append([]MXHost(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pref != out[j].Pref {
			return out[i].Pref < out[j].Pref
		}
		return out[i].Host < out[j].Host
	})
	for i := 0; i < len(out); {
		j := i + 1
		for j < len(out) && out[j].Pref == out[i].Pref {
			j++
		}
		if j-i > 1 {
			grp := out[i:j]
			shuffle(len(grp), func(a, b int) { grp[a], grp[b] = grp[b], grp[a] })
		}
		i = j
	}
	return out
}
