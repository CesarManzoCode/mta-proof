package engine

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/dkim"
)

func writeKey(t *testing.T, dir, domain string, bits int) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	if err := os.WriteFile(filepath.Join(dir, domain+".pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestLoadSigner(t *testing.T) {
	if s, err := LoadSigner(&DKIM{Selector: "lab", Domains: []string{"sender.test"}}); s != nil || err != nil {
		t.Fatalf("no key_dir: %v %v", s, err)
	}
	dir := t.TempDir()
	if _, err := LoadSigner(&DKIM{KeyDir: dir, Selector: "lab", Domains: []string{"sender.test"}}); err == nil {
		t.Fatal("missing key accepted")
	}
	writeKey(t, dir, "short.test", 1024)
	if _, err := LoadSigner(&DKIM{KeyDir: dir, Selector: "lab", Domains: []string{"short.test"}}); err == nil || !strings.Contains(err.Error(), "2048-bit") {
		t.Fatalf("1024-bit key: %v", err)
	}
	for _, d := range []*DKIM{{Selector: ""}, {Selector: "lab", Domains: []string{"Sender.test"}}} {
		if err := d.validate(); err == nil {
			t.Fatalf("%+v accepted", d)
		}
	}
}

// Ingest signs the message it stores, so every attempt sends the same
// bytes; another From domain stays unsigned. The signature verifies with
// the public key.
func TestIngestSigns(t *testing.T) {
	dir := t.TempDir()
	key := writeKey(t, dir, "sender.test", DKIMBits)
	signer, err := LoadSigner(&DKIM{KeyDir: dir, Selector: "lab", Domains: []string{"sender.test"}})
	if err != nil {
		t.Fatal(err)
	}
	e := newSim(t)
	e.core.SetSigner(signer)
	signed := e.core.Message(e.ingest(sub("Hello."), "").MsgID).Raw
	other := sub("Hello.")
	other.From = "app@other.test"
	plain := e.core.Message(e.ingest(other, "").MsgID).Raw
	if !bytes.HasPrefix(signed, []byte("DKIM-Signature: ")) || bytes.Contains(plain, []byte("DKIM-Signature")) {
		t.Fatalf("signed %q / plain %q", signed[:40], plain[:40])
	}
	e.reopen()
	if got := e.core.Message("m00000001").Raw; !bytes.Equal(got, signed) {
		t.Fatal("the stored message is not the signed one")
	}
	pub, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	record := "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(pub)
	verifs, err := dkim.VerifyWithOptions(bytes.NewReader(signed), &dkim.VerifyOptions{
		LookupTXT: func(name string) ([]string, error) {
			if name != "lab._domainkey.sender.test" {
				t.Errorf("lookup %s", name)
			}
			return []string{record}, nil
		}})
	if err != nil || len(verifs) != 1 || verifs[0].Err != nil || verifs[0].Domain != "sender.test" {
		t.Fatalf("verify: %v %+v", err, verifs)
	}
	for _, h := range dkimHeaders {
		if !strings.Contains(strings.ToLower(strings.Join(verifs[0].HeaderKeys, ":")), strings.ToLower(h)) {
			t.Errorf("%s not signed: %v", h, verifs[0].HeaderKeys)
		}
	}
}
