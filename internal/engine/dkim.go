package engine

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/emersion/go-msgauth/dkim"
)

// DKIMBits is the only RSA key size the engine signs with (k2-04).
const DKIMBits = 2048

// DKIM is the signing configuration: the messages whose From domain is in
// Domains are signed rsa-sha256 with the key <key_dir>/<domain>.pem
// (PKCS #8, written for every run by mtasim) under Selector, whose public
// key the lab zone publishes at <selector>._domainkey.<domain>.
type DKIM struct {
	KeyDir   string   `json:"key_dir"`
	Selector string   `json:"selector"`
	Domains  []string `json:"domains"`
}

func (d *DKIM) validate() error {
	if d.Selector == "" || strings.ContainsAny(d.Selector, " ;=") {
		return errors.New("dkim.selector must be a non-empty DNS label")
	}
	for _, dom := range d.Domains {
		if dom == "" || dom != strings.ToLower(dom) {
			return fmt.Errorf("dkim.domains: %q must be a lower-case domain", dom)
		}
	}
	return nil
}

// dkimHeaders are the header fields signed: From (required, R-6376 §5.4)
// and the other fields of the generated message that carry its content
// (§5.4.1), each present once.
var dkimHeaders = []string{"From", "To", "Subject", "Date", "Message-ID",
	"MIME-Version", "Content-Type", "Content-Transfer-Encoding"}

// Signer adds a DKIM-Signature to the messages of its domains.
type Signer struct {
	selector string
	keys     map[string]*rsa.PrivateKey
}

// LoadSigner reads the key of every configured domain; it returns nil
// without a key directory.
func LoadSigner(d *DKIM) (*Signer, error) {
	if d == nil || d.KeyDir == "" {
		return nil, nil
	}
	s := &Signer{selector: d.Selector, keys: map[string]*rsa.PrivateKey{}}
	for _, dom := range d.Domains {
		path := filepath.Join(d.KeyDir, dom+".pem")
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		block, _ := pem.Decode(b)
		if block == nil || block.Type != "PRIVATE KEY" {
			return nil, fmt.Errorf("%s: no PKCS #8 PRIVATE KEY block", path)
		}
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok || rk.N.BitLen() != DKIMBits {
			return nil, fmt.Errorf("%s: want a %d-bit RSA key", path, DKIMBits)
		}
		s.keys[dom] = rk
	}
	return s, nil
}

// Sign returns raw with a DKIM-Signature [R-6376 §5] prepended when the
// From domain has a key, and raw unchanged otherwise. Canonicalization is
// relaxed/relaxed. The signature carries t= from the wall clock, so it is
// only used in real mode.
func (s *Signer) Sign(from string, raw []byte) ([]byte, error) {
	dom := domainOf(from)
	key := s.keys[dom]
	if key == nil {
		return raw, nil
	}
	var out bytes.Buffer
	err := dkim.Sign(&out, bytes.NewReader(raw), &dkim.SignOptions{
		Domain: dom, Selector: s.selector, Signer: key,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed,
		BodyCanonicalization:   dkim.CanonicalizationRelaxed,
		HeaderKeys:             dkimHeaders,
	})
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
