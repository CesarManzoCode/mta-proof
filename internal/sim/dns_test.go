package sim

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

const testZone = `{"records": [
	{"name": "multi.test", "type": "MX", "pref": 20, "host": "mx2.multi.test"},
	{"name": "multi.test", "type": "MX", "pref": 10, "host": "mx1.multi.test"},
	{"name": "mx1.multi.test", "type": "A", "addr": "127.0.1.10"},
	{"name": "mx2.multi.test", "type": "A", "addr": "127.0.1.11"},
	{"name": "nullmx.test", "type": "MX", "pref": 0, "host": "."},
	{"name": "sender.test", "type": "TXT", "text": "v=spf1 ip4:127.0.2.0/24 -all"}
]}`

func serveTestZone(t *testing.T) string {
	t.Helper()
	z, err := ParseZone([]byte(testZone))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ServeDNSZone(z, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	return srv.Addr()
}

func query(t *testing.T, addr, net, name string, qtype uint16) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	c := &dns.Client{Net: net}
	r, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("%s %s %s: %v", net, name, dns.TypeToString[qtype], err)
	}
	return r
}

func TestDNSServesZone(t *testing.T) {
	addr := serveTestZone(t)
	for _, network := range []string{"udp", "tcp"} {
		t.Run(network, func(t *testing.T) {
			r := query(t, addr, network, "multi.test.", dns.TypeMX)
			if !r.Authoritative || r.Rcode != dns.RcodeSuccess || len(r.Answer) != 2 {
				t.Fatalf("MX answer %v", r)
			}
			prefs := map[string]uint16{}
			for _, rr := range r.Answer {
				mx := rr.(*dns.MX)
				prefs[mx.Mx] = mx.Preference
			}
			if prefs["mx1.multi.test."] != 10 || prefs["mx2.multi.test."] != 20 {
				t.Fatalf("MX preferences %v", prefs)
			}

			r = query(t, addr, network, "nullmx.test.", dns.TypeMX)
			if len(r.Answer) != 1 {
				t.Fatalf("null MX answer %v", r)
			}
			if mx := r.Answer[0].(*dns.MX); mx.Preference != 0 || mx.Mx != "." {
				t.Fatalf("null MX is %v", mx)
			}

			r = query(t, addr, network, "MX1.multi.test.", dns.TypeA)
			if len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != "127.0.1.10" {
				t.Fatalf("A answer %v", r)
			}

			r = query(t, addr, network, "sender.test.", dns.TypeTXT)
			if len(r.Answer) != 1 || strings.Join(r.Answer[0].(*dns.TXT).Txt, "") != "v=spf1 ip4:127.0.2.0/24 -all" {
				t.Fatalf("TXT answer %v", r)
			}

			// Existing name without that type: NODATA with the SOA.
			r = query(t, addr, network, "nullmx.test.", dns.TypeA)
			if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 || len(r.Ns) != 1 {
				t.Fatalf("NODATA %v", r)
			}
			if r := query(t, addr, network, "absent.test.", dns.TypeMX); r.Rcode != dns.RcodeNameError {
				t.Fatalf("absent name rcode %d", r.Rcode)
			}
			if r := query(t, addr, network, "example.com.", dns.TypeA); r.Rcode != dns.RcodeRefused {
				t.Fatalf("outside .test rcode %d", r.Rcode)
			}
		})
	}
}

func TestZoneValidation(t *testing.T) {
	for name, tc := range map[string]struct{ zone, want string }{
		"outside .test":    {`{"records": [{"name": "a.example", "type": "A", "addr": "127.0.0.1"}]}`, "not under .test"},
		"public address":   {`{"records": [{"name": "a.test", "type": "A", "addr": "192.0.2.1"}]}`, "loopback"},
		"null MX and more": {`{"records": [{"name": "a.test", "type": "MX", "pref": 0, "host": "."}, {"name": "a.test", "type": "MX", "pref": 10, "host": "mx.a.test"}]}`, "must not have other MX"},
		"null MX pref":     {`{"records": [{"name": "a.test", "type": "MX", "pref": 5, "host": "."}]}`, "preference 0"},
		"MX without pref":  {`{"records": [{"name": "a.test", "type": "MX", "host": "mx.a.test"}]}`, "pref is required"},
	} {
		_, err := ParseZone([]byte(tc.zone))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want %q", name, err, tc.want)
		}
	}
	z, err := LoadZone(filepath.Join("..", "..", "lab", "zone.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(z.Addrs("mx1.gmail-model.test")) != 1 {
		t.Fatal("lab zone lacks mx1.gmail-model.test")
	}
}
