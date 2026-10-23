// Package tlslab generates the test CA of a run and the certificates the
// simulator presents on STARTTLS (contract 2.3, D07, D16): one valid for
// the MX hosts, one with the right names signed by another CA and one
// signed by the test CA for a wrong name. Everything is built with
// crypto/x509 in memory; nothing calls openssl.
package tlslab

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"time"
)

// Certificate kinds the simulator can present (mtasim serve --tls).
const (
	Off       = "off"
	Valid     = "valid"      // signed by the test CA for the MX host names
	OtherCA   = "other-ca"   // the MX host names, signed by a CA the engine does not trust
	WrongName = "wrong-name" // signed by the test CA for WrongHost only
)

// WrongHost is the only name of the wrong-name certificate.
const WrongHost = "wrong-name.test"

// Kinds lists the kinds that present a certificate.
var Kinds = []string{Valid, OtherCA, WrongName}

// validity is generous so a long run never sees a certificate expire.
const validity = 7 * 24 * time.Hour

// CA is a self-signed certificate authority.
type CA struct {
	Cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// NewCA generates a CA called name.
func NewCA(name string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, key: key}, nil
}

// Issue returns a server certificate for hosts signed by the CA.
func (ca *CA) Issue(hosts []string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: hosts[0]},
		DNSNames:     hosts,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// PEM encodes the CA certificate.
func (ca *CA) PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw})
}

// Pool returns a pool that trusts only the CA.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Cert)
	return p
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err)
	}
	return n
}

// Lab is the test CA of one run and the certificate of each kind.
type Lab struct {
	CA    *CA
	Certs map[string]tls.Certificate
}

// NewLab generates the test CA, another CA and the three certificates for
// the MX host names hosts.
func NewLab(hosts []string) (*Lab, error) {
	if len(hosts) == 0 {
		return nil, fmt.Errorf("tlslab: no host names")
	}
	ca, err := NewCA("mta-proof test CA")
	if err != nil {
		return nil, err
	}
	other, err := NewCA("mta-proof untrusted CA")
	if err != nil {
		return nil, err
	}
	l := &Lab{CA: ca, Certs: map[string]tls.Certificate{}}
	for kind, issue := range map[string]func() (tls.Certificate, error){
		Valid:     func() (tls.Certificate, error) { return ca.Issue(hosts) },
		OtherCA:   func() (tls.Certificate, error) { return other.Issue(hosts) },
		WrongName: func() (tls.Certificate, error) { return ca.Issue([]string{WrongHost}) },
	} {
		if l.Certs[kind], err = issue(); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// ServerConfig is the TLS configuration of a server presenting the
// certificate of kind.
func (l *Lab) ServerConfig(kind string) (*tls.Config, error) {
	cert, ok := l.Certs[kind]
	if !ok {
		return nil, fmt.Errorf("tlslab: unknown certificate kind %q", kind)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// WriteCA writes the test CA certificate as PEM to path, the file the
// engine trusts (tls.ca_file).
func (l *Lab) WriteCA(path string) error {
	return os.WriteFile(path, l.CA.PEM(), 0o644)
}

// LoadPool reads a PEM file of CA certificates into a pool.
func LoadPool(path string) (*x509.CertPool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("%s: no PEM certificate", path)
	}
	return p, nil
}
