package sim

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// Zone is the lab DNS zone (lab/zone.json). Every name lives under .test
// (RFC 6761 §6.2) and every address is IPv4 loopback, so nothing resolves
// outside the machine.
type Zone struct {
	TTL     uint32       `json:"ttl"`
	Records []ZoneRecord `json:"records"`

	byName map[string][]ZoneRecord
	exists map[string]bool
}

// ZoneRecord is one resource record. MX uses pref and host (host "." is a
// null MX, RFC 7505 §3), A uses addr, TXT uses text.
type ZoneRecord struct {
	Name string  `json:"name"`
	Type string  `json:"type"`
	Pref *uint16 `json:"pref,omitempty"`
	Host string  `json:"host,omitempty"`
	Addr string  `json:"addr,omitempty"`
	Text string  `json:"text,omitempty"`
}

const zoneApex = "test."

// LoadZone reads and validates a zone file.
func LoadZone(path string) (*Zone, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	z, err := ParseZone(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return z, nil
}

// ParseZone decodes and validates a zone.
func ParseZone(data []byte) (*Zone, error) {
	var z Zone
	if err := strictUnmarshal(data, &z); err != nil {
		return nil, err
	}
	if z.TTL == 0 {
		z.TTL = 300
	}
	z.byName = map[string][]ZoneRecord{}
	z.exists = map[string]bool{zoneApex: true}
	var errs []error
	for i, r := range z.Records {
		where := fmt.Sprintf("record %d (%s %s)", i, r.Name, r.Type)
		if !isTestName(r.Name) {
			errs = append(errs, fmt.Errorf("%s: name is not under .test", where))
			continue
		}
		r.Name = dns.Fqdn(normHost(r.Name))
		r.Type = strings.ToUpper(r.Type)
		switch r.Type {
		case "A":
			a, err := netip.ParseAddr(r.Addr)
			if err != nil || !a.Is4() || !a.IsLoopback() {
				errs = append(errs, fmt.Errorf("%s: addr %q is not an IPv4 loopback address", where, r.Addr))
			}
		case "MX":
			if r.Pref == nil {
				errs = append(errs, fmt.Errorf("%s: pref is required", where))
			}
			if r.Host != "." && !isTestName(r.Host) {
				errs = append(errs, fmt.Errorf("%s: host %q is neither \".\" nor a name under .test", where, r.Host))
			}
			if r.Host != "." {
				r.Host = dns.Fqdn(normHost(r.Host))
			}
		case "TXT":
			if r.Text == "" {
				errs = append(errs, fmt.Errorf("%s: text is required", where))
			}
		default:
			errs = append(errs, fmt.Errorf("%s: unsupported type", where))
		}
		z.Records[i] = r
		z.byName[r.Name] = append(z.byName[r.Name], r)
		for n := r.Name; n != zoneApex; {
			z.exists[n] = true
			_, n, _ = strings.Cut(n, ".")
		}
	}
	// RFC 7505 §3: a null MX is the only MX of its domain, with preference 0.
	for name, rrs := range z.byName {
		mx, null := 0, false
		for _, r := range rrs {
			if r.Type == "MX" {
				mx++
				if r.Host == "." {
					null = true
					if r.Pref != nil && *r.Pref != 0 {
						errs = append(errs, fmt.Errorf("%s: null MX must have preference 0", name))
					}
				}
			}
		}
		if null && mx > 1 {
			errs = append(errs, fmt.Errorf("%s: a domain with a null MX must not have other MX records", name))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &z, nil
}

// Addrs returns the A addresses of a host.
func (z *Zone) Addrs(host string) []netip.Addr {
	var out []netip.Addr
	for _, r := range z.byName[dns.Fqdn(normHost(host))] {
		if r.Type == "A" {
			out = append(out, netip.MustParseAddr(r.Addr))
		}
	}
	return out
}

func (z *Zone) soa() *dns.SOA {
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: zoneApex, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: z.TTL},
		Ns:      "ns.test.",
		Mbox:    "hostmaster.test.",
		Serial:  1,
		Refresh: 3600, Retry: 600, Expire: 86400, Minttl: z.TTL,
	}
}

func (z *Zone) rr(r ZoneRecord) dns.RR {
	hdr := dns.RR_Header{Name: r.Name, Class: dns.ClassINET, Ttl: z.TTL}
	switch r.Type {
	case "A":
		hdr.Rrtype = dns.TypeA
		return &dns.A{Hdr: hdr, A: net.IP(netip.MustParseAddr(r.Addr).AsSlice())}
	case "MX":
		hdr.Rrtype = dns.TypeMX
		return &dns.MX{Hdr: hdr, Preference: *r.Pref, Mx: r.Host}
	default:
		hdr.Rrtype = dns.TypeTXT
		return &dns.TXT{Hdr: hdr, Txt: splitTXT(r.Text)}
	}
}

// splitTXT cuts a TXT value into character-strings of at most 255 octets.
func splitTXT(s string) []string {
	var out []string
	for len(s) > 255 {
		out = append(out, s[:255])
		s = s[255:]
	}
	return append(out, s)
}

// ServeDNS answers authoritatively for test. and refuses everything else.
func (z *Zone) ServeDNS(w dns.ResponseWriter, req *dns.Msg) { w.WriteMsg(z.Answer(req)) }

// Answer builds the reply to req, the same on the network and in process
// (mtadst).
func (z *Zone) Answer(req *dns.Msg) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(req)
	if req.Opcode != dns.OpcodeQuery || len(req.Question) != 1 {
		m.Rcode = dns.RcodeNotImplemented
		return m
	}
	q := req.Question[0]
	name := strings.ToLower(q.Name)
	if q.Qclass != dns.ClassINET || (name != zoneApex && !strings.HasSuffix(name, "."+zoneApex)) {
		m.Rcode = dns.RcodeRefused
		return m
	}
	m.Authoritative = true
	if !z.exists[name] {
		m.Rcode = dns.RcodeNameError
		m.Ns = []dns.RR{z.soa()}
		return m
	}
	if name == zoneApex && (q.Qtype == dns.TypeSOA || q.Qtype == dns.TypeANY) {
		m.Answer = append(m.Answer, z.soa())
	}
	for _, r := range z.byName[name] {
		rr := z.rr(r)
		if q.Qtype == dns.TypeANY || rr.Header().Rrtype == q.Qtype {
			rr.Header().Name = q.Name
			m.Answer = append(m.Answer, rr)
		}
	}
	if len(m.Answer) == 0 {
		m.Ns = []dns.RR{z.soa()}
	}
	return m
}

// DNSServer serves a zone on UDP and TCP at the same address.
type DNSServer struct {
	servers []*dns.Server
	wg      sync.WaitGroup
	errs    chan error
}

// ServeDNSZone starts UDP and TCP servers on addr and returns once both
// listen. Use port 0 only with a host that resolves to one address.
func ServeDNSZone(z *Zone, addr string) (*DNSServer, error) {
	udp, tcp, err := listenUDPTCP(addr)
	if err != nil {
		return nil, err
	}
	s := &DNSServer{errs: make(chan error, 2)}
	s.servers = []*dns.Server{
		{PacketConn: udp, Handler: z, ReadTimeout: 5 * time.Second},
		{Listener: tcp, Handler: z, ReadTimeout: 5 * time.Second},
	}
	for _, srv := range s.servers {
		started := make(chan struct{})
		srv.NotifyStartedFunc = func() { close(started) }
		s.wg.Add(1)
		go func(srv *dns.Server) {
			defer s.wg.Done()
			if err := srv.ActivateAndServe(); err != nil {
				s.errs <- err
			}
		}(srv)
		select {
		case <-started:
		case err := <-s.errs:
			s.Shutdown()
			return nil, err
		}
	}
	return s, nil
}

// listenUDPTCP opens UDP and TCP on the same address. With port 0 the
// kernel picks the UDP port, and that port may be taken for TCP by another
// process, so it tries again with a new port.
func listenUDPTCP(addr string) (net.PacketConn, net.Listener, error) {
	_, port, _ := net.SplitHostPort(addr)
	for try := 0; ; try++ {
		udp, err := net.ListenPacket("udp", addr)
		if err != nil {
			return nil, nil, err
		}
		tcp, err := net.Listen("tcp", udp.LocalAddr().String())
		if err == nil {
			return udp, tcp, nil
		}
		udp.Close()
		if port != "0" || try == 20 {
			return nil, nil, err
		}
	}
}

// Addr returns the UDP address the server listens on.
func (s *DNSServer) Addr() string { return s.servers[0].PacketConn.LocalAddr().String() }

// Shutdown stops both servers.
func (s *DNSServer) Shutdown() {
	for _, srv := range s.servers {
		srv.Shutdown()
	}
	s.wg.Wait()
}

// Names returns the names that own records, sorted (for diagnostics).
func (z *Zone) Names() []string {
	out := make([]string, 0, len(z.byName))
	for n := range z.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
