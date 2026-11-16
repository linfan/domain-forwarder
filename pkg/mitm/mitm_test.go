package mitm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCertificateForDomain(t *testing.T) {
	certFile, keyFile, caCert := newTestCA(t)
	a, err := NewAuthority(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewAuthority: %v", err)
	}

	cert, err := a.CertificateFor("api.example.com")
	if err != nil {
		t.Fatalf("CertificateFor: %v", err)
	}
	if cert.Leaf == nil {
		t.Fatal("Leaf is not populated")
	}
	if got := cert.Leaf.DNSNames; len(got) != 1 || got[0] != "api.example.com" {
		t.Fatalf("DNSNames = %v", got)
	}

	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	if _, err := cert.Leaf.Verify(x509.VerifyOptions{
		Roots:   roots,
		DNSName: "api.example.com",
	}); err != nil {
		t.Fatalf("verify leaf: %v", err)
	}
}

func TestCertificateForIP(t *testing.T) {
	certFile, keyFile, caCert := newTestCA(t)
	a, err := NewAuthority(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewAuthority: %v", err)
	}

	cert, err := a.CertificateFor("127.0.0.1")
	if err != nil {
		t.Fatalf("CertificateFor: %v", err)
	}
	if got := cert.Leaf.IPAddresses; len(got) != 1 || !got[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("IPAddresses = %v", got)
	}

	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	if _, err := cert.Leaf.Verify(x509.VerifyOptions{Roots: roots}); err != nil {
		t.Fatalf("verify leaf: %v", err)
	}
}

func TestCertificateCache(t *testing.T) {
	certFile, keyFile, _ := newTestCA(t)
	a, err := NewAuthority(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewAuthority: %v", err)
	}

	first, err := a.CertificateFor("Cached.Example.com:443")
	if err != nil {
		t.Fatalf("CertificateFor: %v", err)
	}
	second, err := a.CertificateFor("cached.example.com")
	if err != nil {
		t.Fatalf("CertificateFor: %v", err)
	}
	if first != second {
		t.Fatal("expected the cached certificate to be reused")
	}
}

func TestNewAuthorityRejectsNonCA(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "leaf.crt")
	keyFile := filepath.Join(dir, "leaf.key")
	writeSelfSigned(t, certFile, keyFile, false)

	if _, err := NewAuthority(certFile, keyFile); err == nil {
		t.Fatal("expected a non-CA certificate to be rejected")
	}
}

func TestGroupMatch(t *testing.T) {
	certFile, keyFile, _ := newTestCA(t)
	a, err := NewAuthority(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewAuthority: %v", err)
	}
	g, err := NewGroup("test", []string{"exact.example.com", "*.wild.example.com", "127.0.0.1", "*", "::1"}, a)
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}

	matches := []string{
		"exact.example.com:443",
		"EXACT.example.com",
		"a.wild.example.com:443",
		"a.b.wild.example.com",
		"127.0.0.1:8443",
		"[::1]:443",
		"anything.else",
	}
	for _, hostport := range matches {
		if !g.Match(hostport) {
			t.Errorf("Match(%q) = false, want true", hostport)
		}
	}

	noMatch, err := NewGroup("narrow", []string{"exact.example.com", "*.wild.example.com"}, a)
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	for _, hostport := range []string{"other.example.com", "wild.example.com", "sub.exact.example.com", ""} {
		if noMatch.Match(hostport) {
			t.Errorf("Match(%q) = true, want false", hostport)
		}
	}
}

func TestConfigMatchOrder(t *testing.T) {
	certFile, keyFile, _ := newTestCA(t)
	a, err := NewAuthority(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewAuthority: %v", err)
	}
	specific, _ := NewGroup("specific", []string{"*.corp.example"}, a)
	catchAll, _ := NewGroup("all", []string{"*"}, a)
	cfg := &Config{Groups: []*Group{specific, catchAll}}

	got, ok := cfg.Match("api.corp.example:443")
	if !ok || got != specific {
		t.Fatalf("Match(specific) = %v, %v; want specific group", got, ok)
	}
	got, ok = cfg.Match("other.example:443")
	if !ok || got != catchAll {
		t.Fatalf("Match(other) = %v, %v; want catch-all group", got, ok)
	}
}

func TestValidateHostPattern(t *testing.T) {
	valid := []string{"*", "*.example.com", "example.com", "127.0.0.1", "::1", "[::1]"}
	for _, host := range valid {
		if err := ValidateHostPattern(host); err != nil {
			t.Errorf("ValidateHostPattern(%q) = %v, want nil", host, err)
		}
	}
	invalid := []string{"", "ex*.example.com", "example.com:443", "https://example.com", "a/b", "a b"}
	for _, host := range invalid {
		if err := ValidateHostPattern(host); err == nil {
			t.Errorf("ValidateHostPattern(%q) = nil, want error", host)
		}
	}
}

func newTestCA(t *testing.T) (certFile, keyFile string, cert *x509.Certificate) {
	t.Helper()
	dir := t.TempDir()
	certFile = filepath.Join(dir, "ca.crt")
	keyFile = filepath.Join(dir, "ca.key")
	cert = writeSelfSigned(t, certFile, keyFile, true)
	return certFile, keyFile, cert
}

func writeSelfSigned(t *testing.T, certFile, keyFile string, isCA bool) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  isCA,
	}
	if isCA {
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return cert
}
