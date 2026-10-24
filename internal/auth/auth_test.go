package auth_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/CesarManzoCode/mta-proof/internal/auth"
	"github.com/CesarManzoCode/mta-proof/internal/engine"
	"github.com/CesarManzoCode/mta-proof/internal/sim"
	"github.com/emersion/go-msgauth/dkim"
)

// lab is an in-memory zone with its DKIM keys generated, the resolver the
// receiver uses on it and the engine's signer for its DKIM domains.
type lab struct {
	res    *auth.Resolver
	signer *engine.Signer
	dir    string
}

func newLab(t *testing.T, records string, dkimDomains ...string) *lab {
	t.Helper()
	z, err := sim.ParseZone([]byte(`{"ttl": 300, "records": [` + records + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := z.GenerateDKIM(dir); err != nil {
		t.Fatal(err)
	}
	s, err := engine.LoadSigner(&engine.DKIM{KeyDir: dir, Selector: "lab", Domains: dkimDomains})
	if err != nil {
		t.Fatal(err)
	}
	return &lab{res: &auth.Resolver{Exchange: auth.AnswerExchange(z.Answer)}, signer: s, dir: dir}
}

func (l *lab) message(t *testing.T, from string) []byte {
	t.Helper()
	sub := &engine.Submission{Tenant: "acme", From: from, To: "user@outlook-model.test",
		Subject: "auth test", Text: "Hello.\n", MsgType: engine.TypeTransactional}
	raw := engine.BuildMessage("m00000001", sub, time.Unix(1_800_000_000, 0))
	signed, err := l.signer.Sign(from, raw)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// signAs signs raw with the lab key of domain whatever its From says.
func (l *lab) signAs(t *testing.T, domain string, raw []byte) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(l.dir, domain+".pem"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(b)
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := dkim.Sign(&out, bytes.NewReader(raw), &dkim.SignOptions{Domain: domain, Selector: "lab",
		Signer: key.(crypto.Signer), HeaderKeys: []string{"From", "Subject"}}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func (l *lab) check(ip, mailFrom string, msg []byte) auth.Result {
	return auth.Check(context.Background(), l.res, auth.Input{
		IP: net.ParseIP(ip), Helo: "engine.mtaproof.test", MailFrom: mailFrom, Message: msg})
}

const pool = "127.0.2.1"

// The lab zone's two sender configurations (lab/zone.json): good passes
// all three, broken publishes another DKIM key and an SPF record that
// excludes the pool.
const goodAndBroken = `
	{"name": "good.test", "type": "TXT", "text": "v=spf1 ip4:127.0.2.0/24 -all"},
	{"name": "lab._domainkey.good.test", "type": "DKIM", "key": "signing"},
	{"name": "_dmarc.good.test", "type": "TXT", "text": "v=DMARC1; p=reject; adkim=s; aspf=s"},
	{"name": "broken.test", "type": "TXT", "text": "v=spf1 -ip4:127.0.2.0/24 ?all"},
	{"name": "lab._domainkey.broken.test", "type": "DKIM", "key": "other"},
	{"name": "_dmarc.broken.test", "type": "TXT", "text": "v=DMARC1; p=none"},
	{"name": "reject.test", "type": "TXT", "text": "v=spf1 -ip4:127.0.2.0/24 ?all"},
	{"name": "lab._domainkey.reject.test", "type": "DKIM", "key": "other"},
	{"name": "_dmarc.reject.test", "type": "TXT", "text": "v=DMARC1; p=reject"}`

func TestSignAndVerifyGood(t *testing.T) {
	l := newLab(t, goodAndBroken, "good.test", "broken.test", "reject.test")
	msg := l.message(t, "notify@good.test")
	if !bytes.HasPrefix(msg, []byte("DKIM-Signature: ")) {
		t.Fatalf("message not signed: %q", msg[:60])
	}
	hdr := string(msg[:bytes.Index(msg, []byte("\r\nDate:"))])
	for _, tag := range []string{"a=rsa-sha256", "d=good.test", "s=lab", "c=relaxed/relaxed"} {
		if !strings.Contains(strings.Join(strings.Fields(hdr), ""), tag) {
			t.Errorf("signature lacks %s: %s", tag, hdr)
		}
	}
	got := l.check(pool, "notify@good.test", msg)
	if got.SPF != auth.Pass || got.DKIM != auth.Pass || got.DMARC != auth.Pass {
		t.Fatalf("good sender: %+v", got)
	}
	if got.Policy != auth.PolicyReject || got.Author != "good.test" || !slices.Equal(got.DKIMDomains, []string{"good.test"}) {
		t.Fatalf("good sender details: %+v", got)
	}
}

func TestBrokenConfigurationFailsAll(t *testing.T) {
	l := newLab(t, goodAndBroken, "good.test", "broken.test", "reject.test")
	got := l.check(pool, "notify@broken.test", l.message(t, "notify@broken.test"))
	if got.SPF != auth.Fail || got.SPFResult != "fail" || got.DKIM != auth.Fail || got.DMARC != auth.Fail {
		t.Fatalf("broken sender: %+v", got)
	}
	if got.Policy != auth.PolicyNone {
		t.Fatalf("broken sender policy %q, want none", got.Policy)
	}
	got = l.check(pool, "notify@reject.test", l.message(t, "notify@reject.test"))
	if got.DMARC != auth.Fail || got.Policy != auth.PolicyReject {
		t.Fatalf("p=reject sender: %+v", got)
	}
}

func TestDKIMDetectsTampering(t *testing.T) {
	l := newLab(t, goodAndBroken, "good.test")
	msg := l.message(t, "notify@good.test")
	tampered := bytes.Replace(msg, []byte("Hello."), []byte("Hullo."), 1)
	got := l.check(pool, "notify@good.test", tampered)
	if got.DKIM != auth.Fail || got.SPF != auth.Pass || got.DMARC != auth.Pass {
		// SPF still passes and aligns, so DMARC passes without DKIM.
		t.Fatalf("tampered body: %+v", got)
	}
	unsigned := msg[bytes.Index(msg, []byte("Date:")):]
	if got := l.check(pool, "notify@good.test", unsigned); got.DKIM != auth.Fail {
		t.Fatalf("unsigned message: %+v", got)
	}
}

func TestSPF(t *testing.T) {
	l := newLab(t, goodAndBroken+`,
		{"name": "soft.test", "type": "TXT", "text": "v=spf1 ip4:127.0.2.1 ~all"},
		{"name": "inc.test", "type": "TXT", "text": "v=spf1 include:good.test -all"},
		{"name": "two.test", "type": "TXT", "text": "v=spf1 -all"},
		{"name": "two.test", "type": "TXT", "text": "v=spf1 +all"},
		{"name": "helo.test", "type": "TXT", "text": "v=spf1 ip4:127.0.2.9 -all"}`)
	msg := []byte("From: a@x.test\r\n\r\nbody\r\n")
	cases := []struct {
		ip, from, helo, want string
	}{
		{"127.0.2.1", "a@good.test", "", "pass"},
		{"127.0.3.1", "a@good.test", "", "fail"},
		{"127.0.2.1", "a@broken.test", "", "fail"},
		{"127.0.2.2", "a@soft.test", "", "softfail"},
		{"127.0.2.7", "a@inc.test", "", "pass"},  // R-7208 §5.2: include matches on pass
		{"127.0.2.7", "a@none.test", "", "none"}, // no record: NXDOMAIN (§4.3)
		{"127.0.2.7", "a@two.test", "", "permerror"},
		{"127.0.2.9", "", "helo.test", "pass"}, // null reverse-path: the HELO identity
	}
	for _, c := range cases {
		helo := c.helo
		if helo == "" {
			helo = "engine.mtaproof.test"
		}
		got := auth.Check(context.Background(), l.res, auth.Input{IP: net.ParseIP(c.ip), Helo: helo, MailFrom: c.from, Message: msg})
		if got.SPFResult != c.want {
			t.Errorf("%s from %s: SPF %s, want %s", c.ip, c.from, got.SPFResult, c.want)
		}
		if (got.SPF == auth.Pass) != (c.want == "pass") {
			t.Errorf("%s from %s: logged %s", c.ip, c.from, got.SPF)
		}
	}
}

func TestDMARCTreeWalkAndAlignment(t *testing.T) {
	l := newLab(t, `
		{"name": "_dmarc.example.test", "type": "TXT", "text": "v=DMARC1; p=quarantine; sp=reject; np=none"},
		{"name": "mail.example.test", "type": "TXT", "text": "v=spf1 ip4:127.0.2.0/24 -all"},
		{"name": "a.b.mail.example.test", "type": "A", "addr": "127.0.0.9"},
		{"name": "lab._domainkey.example.test", "type": "DKIM", "key": "signing"},
		{"name": "_dmarc.strict.test", "type": "TXT", "text": "v=DMARC1; p=reject; adkim=s; aspf=s"},
		{"name": "sub.strict.test", "type": "TXT", "text": "v=spf1 ip4:127.0.2.0/24 -all"},
		{"name": "_dmarc.org.test", "type": "TXT", "text": "v=DMARC1; p=reject; psd=y"},
		{"name": "shop.org.test", "type": "TXT", "text": "v=spf1 ip4:127.0.2.0/24 -all"},
		{"name": "other.org.test", "type": "TXT", "text": "v=spf1 ip4:127.0.2.0/24 -all"},
		{"name": "mail.shop.org.test", "type": "A", "addr": "127.0.0.9"},
		{"name": "_dmarc.bad.test", "type": "TXT", "text": "v=DMARC1; p=bogus"},
		{"name": "_dmarc.twice.test", "type": "TXT", "text": "v=DMARC1; p=reject"},
		{"name": "_dmarc.twice.test", "type": "TXT", "text": "v=DMARC1; p=none"},
		{"name": "_dmarc.testing.test", "type": "TXT", "text": "v=DMARC1; p=reject; t=y"},
		{"name": "testing.test", "type": "TXT", "text": "v=spf1 -all"}`, "example.test")
	body := func(from string) []byte { return []byte("From: " + from + "\r\nSubject: s\r\n\r\nbody\r\n") }
	cases := []struct {
		name, mailFrom, from string
		msg                  []byte
		dmarc, policy        string
	}{
		// §4.10.1: no record at the Author Domain; the walk finds the
		// Organizational Domain's record and an existing subdomain gets sp.
		{"relaxed SPF, existing subdomain", "x@mail.example.test", "n@a.b.mail.example.test", nil, auth.Pass, auth.PolicyReject},
		// A subdomain that does not exist gets np.
		{"no identifier, missing subdomain", "x@nowhere.test", "n@gone.example.test", nil, auth.Fail, auth.PolicyNone},
		// A DKIM d=example.test aligns relaxed with mail.example.test.
		{"relaxed DKIM", "x@nowhere.test", "n@mail.example.test", l.signAs(t, "example.test", body("n@mail.example.test")), auth.Pass, auth.PolicyReject},
		// Strict alignment needs identical domains.
		{"strict SPF", "x@sub.strict.test", "n@strict.test", nil, auth.Fail, auth.PolicyReject},
		// psd=y: the Organizational Domain is one label below the PSD.
		{"psd aligned", "x@shop.org.test", "n@mail.shop.org.test", nil, auth.Pass, auth.PolicyReject},
		{"psd not aligned", "x@other.org.test", "n@mail.shop.org.test", nil, auth.Fail, auth.PolicyReject},
		// Invalid p without rua, two records: no DMARC.
		{"invalid record", "x@nowhere.test", "n@bad.test", nil, auth.Fail, ""},
		{"two records", "x@nowhere.test", "n@twice.test", nil, auth.Fail, ""},
		// t=y lowers reject to quarantine.
		{"testing mode", "x@testing.test", "n@testing.test", nil, auth.Fail, auth.PolicyQuarantine},
	}
	for _, c := range cases {
		msg := c.msg
		if msg == nil {
			msg = body(c.from)
		}
		got := l.check(pool, c.mailFrom, msg)
		if got.DMARC != c.dmarc || got.Policy != c.policy {
			t.Errorf("%s: dmarc %s policy %q, want %s %q (%+v)", c.name, got.DMARC, got.Policy, c.dmarc, c.policy, got)
		}
	}
}

func TestDMARCNeedsOneFrom(t *testing.T) {
	l := newLab(t, `{"name": "_dmarc.example.test", "type": "TXT", "text": "v=DMARC1; p=reject"},
		{"name": "example.test", "type": "TXT", "text": "v=spf1 ip4:127.0.2.0/24 -all"}`)
	for _, msg := range []string{
		"From: a@example.test\r\nFrom: b@example.test\r\n\r\nx\r\n",
		"From: a@example.test, b@example.test\r\n\r\nx\r\n",
		"Subject: no from\r\n\r\nx\r\n",
	} {
		got := l.check(pool, "a@example.test", []byte(msg))
		if got.DMARC != auth.Fail || got.Policy != "" || got.SPF != auth.Pass {
			t.Errorf("%q: %+v", msg, got)
		}
	}
}

func TestParseRecord(t *testing.T) {
	cases := []struct {
		txt string
		ok  bool
		p   string
	}{
		{"v=DMARC1; p=reject", true, "reject"},
		{"v=DMARC1;p=quarantine;sp=none", true, "quarantine"},
		{"p=reject; v=DMARC1", false, ""},
		{"v=dmarc1; p=reject", false, ""},
		{"v=DMARC1", false, ""},
		{"v=DMARC1; rua=mailto:r@example.test", true, "none"},
		{"v=DMARC1; p=bogus; rua=mailto:r@example.test", true, "none"},
		{"v=DMARC1; p=reject; sp=bogus", false, ""},
		{"v=DMARC1; p=REJECT; unknown=1", true, "reject"},
	}
	for _, c := range cases {
		rec, ok := auth.ParseRecord(c.txt)
		if ok != c.ok || ok && rec.P != c.p {
			t.Errorf("%q: %v %+v, want %v p=%s", c.txt, ok, rec, c.ok, c.p)
		}
	}
}
