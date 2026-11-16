package mitm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCertificateForDomain(t *testing.T) {
	certFile, keyFile, caCert := newTestCA(t)
	a, err := NewAuthority(certFile, keyFile, 0)
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
	a, err := NewAuthority(certFile, keyFile, 0)
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
	a, err := NewAuthority(certFile, keyFile, 0)
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

func TestRenewAtMargin(t *testing.T) {
	now := time.Now()

	// A full seven-day leaf is replaced a day before it expires.
	long := now.Add(leafValidity)
	if got := renewAt(now, long); !got.Equal(long.Add(-leafRenewWindow)) {
		t.Errorf("renewAt(7d) = %v, want %v", got, long.Add(-leafRenewWindow))
	}

	// A leaf clamped by a short-lived CA keeps two thirds of its lifetime, so
	// it is not re-minted on every request.
	short := now.Add(90 * time.Minute)
	if got := renewAt(now, short); !got.Equal(short.Add(-30 * time.Minute)) {
		t.Errorf("renewAt(90m) = %v, want %v", got, short.Add(-30*time.Minute))
	}
}

func TestCertificateReMintedAfterRenewDeadline(t *testing.T) {
	certFile, keyFile, caCert := newTestCA(t)
	a, err := NewAuthority(certFile, keyFile, 0)
	if err != nil {
		t.Fatalf("NewAuthority: %v", err)
	}

	first, err := a.CertificateFor("renew.example.com")
	if err != nil {
		t.Fatalf("CertificateFor: %v", err)
	}

	// Pretend the renew deadline passed: an expired leaf must never be handed
	// out again, or a client would reject it on a long-running tunnel.
	a.mu.Lock()
	a.cache["renew.example.com"].renewAt = time.Now().Add(-time.Second)
	a.mu.Unlock()

	second, err := a.CertificateFor("renew.example.com")
	if err != nil {
		t.Fatalf("renew CertificateFor: %v", err)
	}
	if second == first {
		t.Fatal("expected a re-minted leaf once the renew deadline passed")
	}
	if second.Leaf.SerialNumber.Cmp(first.Leaf.SerialNumber) == 0 {
		t.Fatal("the renewed leaf is the same certificate, not a newly minted one")
	}

	third, err := a.CertificateFor("renew.example.com")
	if err != nil {
		t.Fatalf("third CertificateFor: %v", err)
	}
	if third != second {
		t.Fatal("the renewed leaf must be cached again")
	}

	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	if _, err := third.Leaf.Verify(x509.VerifyOptions{Roots: roots}); err != nil {
		t.Fatalf("verify renewed leaf: %v", err)
	}
}

func TestCacheHonoursMaxCachedHosts(t *testing.T) {
	certFile, keyFile, caCert := newTestCA(t)
	const limit = 4
	a, err := NewAuthority(certFile, keyFile, limit)
	if err != nil {
		t.Fatalf("NewAuthority: %v", err)
	}

	for i := 0; i < 3*limit; i++ {
		if _, err := a.CertificateFor(fmt.Sprintf("host%d.example.com", i)); err != nil {
			t.Fatalf("CertificateFor host %d: %v", i, err)
		}
	}

	a.mu.Lock()
	size := len(a.cache)
	a.mu.Unlock()
	if size > limit {
		t.Fatalf("cache holds %d hosts, limit is %d", size, limit)
	}

	// Eviction may only drop a leaf, never break the host it belongs to.
	leaf, err := a.CertificateFor("host0.example.com")
	if err != nil {
		t.Fatalf("CertificateFor after eviction: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{Roots: roots}); err != nil {
		t.Fatalf("verify evicted-and-reminted leaf: %v", err)
	}
}

func TestNewAuthorityRejectsNonCA(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "leaf.crt")
	keyFile := filepath.Join(dir, "leaf.key")
	writeSelfSigned(t, certFile, keyFile, false)

	if _, err := NewAuthority(certFile, keyFile, 0); err == nil {
		t.Fatal("expected a non-CA certificate to be rejected")
	}
}

func TestGroupMatch(t *testing.T) {
	certFile, keyFile, _ := newTestCA(t)
	a, err := NewAuthority(certFile, keyFile, 0)
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
	a, err := NewAuthority(certFile, keyFile, 0)
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

func TestTunnelExclusionMatching(t *testing.T) {
	cases := []struct {
		entries   []string
		authority string
		want      string // "" means the list does not cover the CONNECT
	}{
		{[]string{"*:25"}, "mail.example.com:25", "*:25"},
		{[]string{"*:25"}, "MAIL.example.com.:25", "*:25"},
		{[]string{"*:25"}, "mail.example.com:443", ""},
		{[]string{"*:25"}, "user:pw@mail.example.com:25", "*:25"},
		// A portless authority carries no port, so an entry that names one must
		// not claim it and interception stays strict.
		{[]string{"*:25"}, "mail.example.com", ""},
		{[]string{"*.example.com:25"}, "a.example.com:25", "*.example.com:25"},
		{[]string{"*.example.com:25"}, "example.com:25", ""},
		{[]string{"legacy.example.com"}, "legacy.example.com:8443", "legacy.example.com"},
		{[]string{"legacy.example.com"}, "legacy.example.com", "legacy.example.com"},
		{[]string{"10.0.0.5:143"}, "10.0.0.5:143", "10.0.0.5:143"},
		{[]string{"10.0.0.5:143"}, "10.0.0.5:993", ""},
		{[]string{"[::1]:143"}, "[::1]:143", "[::1]:143"},
		{[]string{"::1"}, "[::1]:143", "::1"},
		// Ports compare numerically, so a zero-padded entry still matches.
		{[]string{"*:025"}, "mail.example.com:25", "*:025"},
		// The first matching entry is the one reported.
		{[]string{"a.example.com:25", "*:25"}, "a.example.com:25", "a.example.com:25"},
	}
	for _, tc := range cases {
		tunnel, err := NewTunnelExclusion(tc.entries)
		if err != nil {
			t.Fatalf("NewTunnelExclusion(%q): %v", tc.entries, err)
		}
		if got := tunnel.EntryFor(tc.authority); got != tc.want {
			t.Errorf("entries %q EntryFor(%q) = %q, want %q", tc.entries, tc.authority, got, tc.want)
		}
	}
}

func TestTunnelExclusionRejectsBadEntries(t *testing.T) {
	invalid := []string{
		"", "*:*", "example.com:0", "example.com:65536", "example.com:https",
		"example.com:", "https://example.com:25", "*.example.com:*", "a b:25",
		"example.com:25:26", "*example.com:25", "a/b:25",
	}
	for _, entry := range invalid {
		if err := ValidateConnectEntry(entry); err == nil {
			t.Errorf("ValidateConnectEntry(%q) = nil, want an error", entry)
		}
	}

	valid := []string{
		"*", "*.example.com", "example.com", "127.0.0.1", "::1", "[::1]",
		"*:25", "*.example.com:25", "example.com:443", "10.0.0.5:143", "[::1]:143", "*:65535",
	}
	for _, entry := range valid {
		if err := ValidateConnectEntry(entry); err != nil {
			t.Errorf("ValidateConnectEntry(%q) = %v, want nil", entry, err)
		}
	}
}

// TestExcludedCoversGroupMatchedAuthority pins the contract handleConnect
// relies on: a group can cover a host while an exclusion keeps its CONNECT raw.
func TestExcludedCoversGroupMatchedAuthority(t *testing.T) {
	certFile, keyFile, _ := newTestCA(t)
	a, err := NewAuthority(certFile, keyFile, 0)
	if err != nil {
		t.Fatalf("NewAuthority: %v", err)
	}
	catchAll, err := NewGroup("all", []string{"*"}, a)
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	tunnel, err := NewTunnelExclusion([]string{"*:25"})
	if err != nil {
		t.Fatalf("NewTunnelExclusion: %v", err)
	}
	cfg := &Config{Groups: []*Group{catchAll}, Tunnel: tunnel}

	if _, ok := cfg.Match("mail.example.com:25"); !ok {
		t.Error("the catch-all group must still cover mail.example.com:25")
	}
	if got := cfg.Excluded("mail.example.com:25"); got != "*:25" {
		t.Errorf("Excluded = %q, want *:25", got)
	}
	if got := cfg.Excluded("api.example.com:443"); got != "" {
		t.Errorf("Excluded = %q, want empty so the host is intercepted", got)
	}

	var absent *Config
	if got := absent.Excluded("mail.example.com:25"); got != "" {
		t.Errorf("(*Config)(nil).Excluded = %q, want empty", got)
	}
	if got := (&Config{}).Excluded("mail.example.com:25"); got != "" {
		t.Errorf("Config without exclusions Excluded = %q, want empty", got)
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
