package sim

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
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
	for name, tc := range map[string]struct{ zone, want string }{
		"DKIM name":       {`{"records": [{"name": "lab.a.test", "type": "DKIM", "key": "signing"}]}`, "<selector>._domainkey.<domain>"},
		"DKIM twice":      {`{"records": [{"name": "s1._domainkey.a.test", "type": "DKIM", "key": "signing"}, {"name": "s2._domainkey.a.test", "type": "DKIM", "key": "other"}]}`, "one DKIM record per domain"},
		"DKIM key kind":   {`{"records": [{"name": "s1._domainkey.a.test", "type": "DKIM", "key": "mine"}]}`, "key must be"},
		"DKIM text":       {`{"records": [{"name": "s1._domainkey.a.test", "type": "DKIM", "key": "signing", "text": "v=DKIM1; p=x"}]}`, "generated, not given"},
		"underscore host": {`{"records": [{"name": "a.test", "type": "MX", "pref": 10, "host": "_mx.a.test"}]}`, "neither"},
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

// A DKIM record has no answer until GenerateDKIM; then it is a TXT record
// with the published key, cut into 255-octet strings, and the signing key
// of "signing" is the published one while "other" publishes another key.
func TestZoneDKIM(t *testing.T) {
	z, err := ParseZone([]byte(`{"records": [
		{"name": "lab._domainkey.good.test", "type": "DKIM", "key": "signing"},
		{"name": "lab._domainkey.bad.test", "type": "DKIM", "key": "other"},
		{"name": "_dmarc.good.test", "type": "TXT", "text": "v=DMARC1; p=none"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ask := func(name string) *dns.Msg {
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeTXT)
		return z.Answer(q)
	}
	if r := ask("lab._domainkey.good.test."); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
		t.Fatalf("before generation: %v", r)
	}
	if r := ask("_dmarc.good.test."); len(r.Answer) != 1 {
		t.Fatalf("underscore TXT name: %v", r)
	}
	dir := t.TempDir()
	if err := z.GenerateDKIM(dir); err != nil {
		t.Fatal(err)
	}
	published := map[string]string{}
	for _, d := range []string{"good", "bad"} {
		r := ask("lab._domainkey." + d + ".test.")
		if len(r.Answer) != 1 {
			t.Fatalf("%s: %v", d, r)
		}
		txt := r.Answer[0].(*dns.TXT).Txt
		if len(txt) < 2 || len(txt[0]) != 255 {
			t.Fatalf("%s: a 2048-bit key record should span two strings: %v", d, txt)
		}
		published[d] = strings.Join(txt, "")
		if !strings.HasPrefix(published[d], "v=DKIM1; k=rsa; p=") {
			t.Fatalf("%s: %s", d, published[d])
		}
	}
	for d, want := range map[string]bool{"good": true, "bad": false} {
		b, err := os.ReadFile(filepath.Join(dir, d+".test.pem"))
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(b)
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		rk := key.(*rsa.PrivateKey)
		if rk.N.BitLen() != DKIMBits {
			t.Fatalf("%s: %d bits", d, rk.N.BitLen())
		}
		pub, _ := x509.MarshalPKIXPublicKey(&rk.PublicKey)
		if got := published[d] == "v=DKIM1; k=rsa; p="+base64.StdEncoding.EncodeToString(pub); got != want {
			t.Fatalf("%s: published key is the signing key: %v, want %v", d, got, want)
		}
	}
}
