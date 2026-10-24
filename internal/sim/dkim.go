package sim

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
)

// DKIMBits is the RSA key size of the lab's DKIM keys; R-6376 §3.3.3 asks
// signers for at least 1024 bits and the run uses 2048.
const DKIMBits = 2048

// GenerateDKIM creates a new RSA key pair for every DKIM record of the
// zone, writes the signing private key of each domain to <dir>/<domain>.pem
// (PKCS #8) for the engine, and publishes "v=DKIM1; k=rsa; p=<key>"
// [R-6376 §3.6.1]: the signing key's public half for key "signing", the
// public half of another new pair for key "other". The keys exist only for
// this run.
func (z *Zone) GenerateDKIM(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, r := range z.Records {
		if r.Type != "DKIM" {
			continue
		}
		_, domain, _ := strings.Cut(strings.TrimSuffix(r.Name, "."), "._domainkey.")
		signing, err := rsa.GenerateKey(rand.Reader, DKIMBits)
		if err != nil {
			return err
		}
		der, err := x509.MarshalPKCS8PrivateKey(signing)
		if err != nil {
			return err
		}
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		if err := os.WriteFile(filepath.Join(dir, domain+".pem"), pemBytes, 0o600); err != nil {
			return err
		}
		published := signing
		if r.Key == DKIMOther {
			if published, err = rsa.GenerateKey(rand.Reader, DKIMBits); err != nil {
				return err
			}
		}
		pub, err := x509.MarshalPKIXPublicKey(&published.PublicKey)
		if err != nil {
			return err
		}
		z.dkim[r.Name] = "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(pub)
	}
	return nil
}

// HasDKIM reports whether the zone declares DKIM records.
func (z *Zone) HasDKIM() bool {
	for _, r := range z.Records {
		if r.Type == "DKIM" {
			return true
		}
	}
	return false
}
