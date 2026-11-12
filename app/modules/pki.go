package modules

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"sort"
	"strings"
	"time"
)

// CertKeyPair is a PEM-encoded certificate with its private key.
type CertKeyPair struct {
	CertPEM string
	KeyPEM  string
	Cert    *x509.Certificate
}

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

func encodeKey(key *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// GenerateCA creates a new self-signed private CA.
func GenerateCA(now time.Time) (*CertKeyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: fmt.Sprintf("%s private CA %s", AppName, now.UTC().Format("2006-01-02")), Organization: []string{AppName}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(CAValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return finishPair(der, key)
}

// GenerateServerCert creates a server certificate signed by ca for names
// (host names and IP addresses). Loopback addresses are always included.
func GenerateServerCert(ca *CertKeyPair, names []string, now time.Time) (*CertKeyPair, error) {
	caKey, err := ParsePrivateKey(ca.KeyPEM)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	notAfter := now.Add(ServerCertValidity)
	if notAfter.After(ca.Cert.NotAfter) {
		notAfter = ca.Cert.NotAfter
	}
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: AppName + " central", Organization: []string{AppName}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, n := range ServerCertNames(names) {
		if ip := net.ParseIP(n); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.Cert, &key.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	return finishPair(der, key)
}

// NormalizeCertName returns the canonical form of a certificate name: the
// textual form of an IP address, or a host name in lower case.
func NormalizeCertName(n string) string {
	n = strings.TrimSpace(n)
	if ip := net.ParseIP(n); ip != nil {
		return ip.String()
	}
	return strings.ToLower(n)
}

// ServerCertNames returns the configured names plus the loopback names,
// normalized (NormalizeCertName), de-duplicated and sorted.
func ServerCertNames(names []string) []string {
	set := map[string]bool{"127.0.0.1": true, "::1": true, "localhost": true}
	for _, n := range names {
		if n = NormalizeCertName(n); n != "" {
			set[n] = true
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// CertNames returns the DNS and IP names of a certificate, sorted.
func CertNames(c *x509.Certificate) []string {
	var out []string
	out = append(out, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		out = append(out, ip.String())
	}
	sort.Strings(out)
	return out
}

func finishPair(der []byte, key *ecdsa.PrivateKey) (*CertKeyPair, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	return &CertKeyPair{
		CertPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		KeyPEM:  keyPEM,
		Cert:    cert,
	}, nil
}

// ParseCertPEM parses the first certificate in a PEM string.
func ParseCertPEM(s string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no certificate in PEM data")
	}
	return x509.ParseCertificate(block.Bytes)
}

// ParsePrivateKey parses a PKCS#8 PEM private key.
func ParsePrivateKey(s string) (any, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, errors.New("no key in PEM data")
	}
	return x509.ParsePKCS8PrivateKey(block.Bytes)
}

// LoadPair parses a stored certificate and key into a pair.
func LoadPair(certPEM, keyPEM string) (*CertKeyPair, error) {
	c, err := ParseCertPEM(certPEM)
	if err != nil {
		return nil, err
	}
	return &CertKeyPair{CertPEM: certPEM, KeyPEM: keyPEM, Cert: c}, nil
}

// TLSCertificate converts the pair (plus intermediate-free chain) for tls.
func (p *CertKeyPair) TLSCertificate() (*tls.Certificate, error) {
	c, err := tls.X509KeyPair([]byte(p.CertPEM), []byte(p.KeyPEM))
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Fingerprint returns the SHA-256 fingerprint (lowercase hex) of a certificate.
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return hex.EncodeToString(sum[:])
}

// FormatFingerprint renders a fingerprint as colon separated upper-case pairs.
func FormatFingerprint(fp string) string {
	fp = strings.ToUpper(strings.ReplaceAll(fp, ":", ""))
	var parts []string
	for i := 0; i+2 <= len(fp); i += 2 {
		parts = append(parts, fp[i:i+2])
	}
	return strings.Join(parts, ":")
}

// NormalizeFingerprint removes separators and lower-cases a fingerprint.
func NormalizeFingerprint(fp string) string {
	return strings.ToLower(strings.NewReplacer(":", "", " ", "", "-", "").Replace(strings.TrimSpace(fp)))
}

// CertPoolFromPEMs builds a pool from PEM certificates, skipping expired ones.
func CertPoolFromPEMs(pems []string, now time.Time) (*x509.CertPool, int) {
	pool := x509.NewCertPool()
	n := 0
	for _, p := range pems {
		c, err := ParseCertPEM(p)
		if err != nil || now.After(c.NotAfter) {
			continue
		}
		pool.AddCert(c)
		n++
	}
	return pool, n
}

// AgentTLSConfig returns a client TLS config that trusts only the given CAs.
func AgentTLSConfig(caPEMs []string) (*tls.Config, error) {
	pool, n := CertPoolFromPEMs(caPEMs, time.Now())
	if n == 0 {
		return nil, errors.New("no valid trusted CA certificate")
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}
