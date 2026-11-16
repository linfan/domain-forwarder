// Package mitm mints per-host TLS certificates from configured authorities and
// maps CONNECT targets to interception groups.
package mitm

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
)

// leafValidity bounds how long a minted leaf stays valid. Cached leaves are
// replaced before they expire (see renewAt), so a proxy that runs for weeks
// never presents a certificate a client rejects.
const leafValidity = 7 * 24 * time.Hour

// leafRenewWindow caps how early a cached leaf is re-minted. The actual margin
// is a third of the leaf's lifetime (see renewAt), so a CA with a short validity
// period does not turn every request into a signing operation.
const leafRenewWindow = 24 * time.Hour

// DefaultMaxCachedHosts is the number of leaves one Authority caches at most.
// A cached leaf costs tens of kilobytes, so an unbounded per-host cache grows
// with the traffic mix instead of staying proportional to the live tunnels.
const DefaultMaxCachedHosts = 4096

// Authority issues leaf certificates for individual hosts, signed by one CA and
// cached per host.
type Authority struct {
	cert   *x509.Certificate
	chain  [][]byte
	signer crypto.Signer

	maxCachedHosts int
	mu             sync.Mutex
	cache          map[string]*cachedLeaf
}

// cachedLeaf is one minted leaf plus the moment it must be replaced.
type cachedLeaf struct {
	cert    *tls.Certificate
	renewAt time.Time
}

// NewAuthority loads the CA certificate/key pair used to sign minted leaves.
// maxCachedHosts bounds how many leaves are kept; 0 or less disables the
// limit, which is what an unlimited cache means in the configuration.
func NewAuthority(certFile, keyFile string, maxCachedHosts int) (*Authority, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load CA certificate/key: %w", err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate %s: %w", certFile, err)
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("CA certificate %s is not a CA certificate", certFile)
	}
	if cert.KeyUsage != 0 && cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, fmt.Errorf("CA certificate %s lacks the certificate-signing key usage", certFile)
	}
	if time.Now().After(cert.NotAfter) {
		return nil, fmt.Errorf("CA certificate %s expired on %s", certFile, cert.NotAfter.Format(time.RFC3339))
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("CA key %s cannot sign certificates", keyFile)
	}
	return &Authority{
		cert:           cert,
		chain:          pair.Certificate,
		signer:         signer,
		maxCachedHosts: maxCachedHosts,
		cache:          make(map[string]*cachedLeaf),
	}, nil
}

// CertificateFor returns a leaf certificate for host, minting and caching it on
// first use. host may be a DNS name or an IP literal. A cached leaf is reused
// until its renewAt deadline passes, so a host keeps its certificate while a
// long-running proxy never hands out an expiring one.
func (a *Authority) CertificateFor(host string) (*tls.Certificate, error) {
	host = normalizeHost(host)
	if host == "" {
		return nil, fmt.Errorf("empty host")
	}

	now := time.Now()
	a.mu.Lock()
	cached := a.cache[host]
	a.mu.Unlock()
	if cached != nil && cached.renewAt.After(now) {
		return cached.cert, nil
	}

	cert, err := a.issue(host)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	// Another goroutine may have minted the same host while the lock was dropped.
	if existing := a.cache[host]; existing != nil && existing.renewAt.After(time.Now()) {
		return existing.cert, nil
	}
	a.store(host, cert)
	return cert, nil
}

// store caches one leaf and keeps the cache within maxCachedHosts.
func (a *Authority) store(host string, cert *tls.Certificate) {
	a.cache[host] = &cachedLeaf{cert: cert, renewAt: renewAt(cert.Leaf.NotBefore, cert.Leaf.NotAfter)}
	if a.maxCachedHosts <= 0 || len(a.cache) <= a.maxCachedHosts {
		return
	}
	// Over the limit, drop one entry. Map iteration order is randomized, so this
	// is an O(1) approximate eviction: the evicted host re-mints on its next use,
	// which is also what an expired leaf would have required.
	for name := range a.cache {
		delete(a.cache, name)
		break
	}
}

// renewAt is when a leaf minted to be valid from notBefore until notAfter has
// lost enough of its lifetime to be worth replacing. The margin is a third of
// the lifetime, never more than leafRenewWindow: re-minting before expiry keeps
// tunnels that outlive a certificate from ever seeing it expire.
func renewAt(notBefore, notAfter time.Time) time.Time {
	margin := notAfter.Sub(notBefore) / 3
	if margin > leafRenewWindow {
		margin = leafRenewWindow
	}
	return notAfter.Add(-margin)
}

func (a *Authority) issue(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key for %s: %w", host, err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial for %s: %w", host, err)
	}

	now := time.Now()
	notAfter := now.Add(leafValidity)
	if a.cert.NotAfter.Before(notAfter) {
		notAfter = a.cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.signer)
	if err != nil {
		return nil, fmt.Errorf("issue certificate for %s: %w", host, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse issued certificate for %s: %w", host, err)
	}
	return &tls.Certificate{
		Certificate: append([][]byte{der}, a.chain...),
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}

// Group pairs a name and host patterns with the CA that signs their certificates.
// Patterns are evaluated in order, and the first matching group of a Config wins.
type Group struct {
	Name string
	CA   *Authority

	hosts    []string
	patterns []pattern
}

type pattern struct {
	matchAll bool
	suffix   string // ".example.com" for "*.example.com"
	exact    string
}

// NewGroup builds a group from validated host patterns.
func NewGroup(name string, hosts []string, ca *Authority) (*Group, error) {
	if ca == nil {
		return nil, fmt.Errorf("group %q has no CA", name)
	}
	g := &Group{Name: name, CA: ca, hosts: append([]string(nil), hosts...)}
	for _, host := range hosts {
		p, err := parsePattern(host)
		if err != nil {
			return nil, fmt.Errorf("group %q: %w", name, err)
		}
		g.patterns = append(g.patterns, p)
	}
	if len(g.patterns) == 0 {
		return nil, fmt.Errorf("group %q has no hosts", name)
	}
	return g, nil
}

// Hosts returns the configured host patterns, for logging.
func (g *Group) Hosts() []string { return append([]string(nil), g.hosts...) }

// Match reports whether hostport is covered by this group. The port is ignored
// and matching is case-insensitive.
func (g *Group) Match(hostport string) bool {
	host := normalizeHost(hostport)
	if host == "" {
		return false
	}
	for _, p := range g.patterns {
		switch {
		case p.matchAll:
			return true
		case p.suffix != "":
			if strings.HasSuffix(host, p.suffix) && len(host) > len(p.suffix) {
				return true
			}
		case p.exact == host:
			return true
		}
	}
	return false
}

// Config is an ordered list of interception groups.
type Config struct {
	Groups []*Group
}

// Match returns the first group covering hostport.
func (c *Config) Match(hostport string) (*Group, bool) {
	if c == nil {
		return nil, false
	}
	for _, g := range c.Groups {
		if g.Match(hostport) {
			return g, true
		}
	}
	return nil, false
}

// ValidateHostPattern checks one host pattern: "*", "*.example.com", a bare
// host name or an IP literal.
func ValidateHostPattern(host string) error {
	_, err := parsePattern(host)
	return err
}

func parsePattern(host string) (pattern, error) {
	if host == "" {
		return pattern{}, fmt.Errorf("host pattern must not be empty")
	}
	if strings.ContainsAny(host, " \t\r\n") {
		return pattern{}, fmt.Errorf("invalid host pattern %q: must not contain whitespace", host)
	}
	if host == "*" {
		return pattern{matchAll: true}, nil
	}
	if rest, ok := strings.CutPrefix(host, "*."); ok {
		suffix := strings.ToLower(strings.TrimSuffix(rest, "."))
		if suffix == "" || strings.ContainsAny(rest, "*:/@?#[]") {
			return pattern{}, fmt.Errorf("invalid host pattern %q: wildcard must look like *.example.com", host)
		}
		if net.ParseIP(suffix) != nil {
			return pattern{}, fmt.Errorf("invalid host pattern %q: wildcard cannot cover an IP literal", host)
		}
		return pattern{suffix: "." + suffix}, nil
	}
	if strings.Contains(host, "*") {
		return pattern{}, fmt.Errorf("invalid host pattern %q: wildcard is only allowed as a leading * or *.", host)
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return pattern{exact: ip.String()}, nil
	}
	if strings.ContainsAny(host, "/:@?#[]") {
		return pattern{}, fmt.Errorf("invalid host pattern %q: must be a bare host name without scheme, port or path", host)
	}
	exact := strings.ToLower(strings.TrimSuffix(host, "."))
	if exact == "" {
		return pattern{}, fmt.Errorf("invalid host pattern %q", host)
	}
	return pattern{exact: exact}, nil
}

// Host returns the bare, lowercased host of a host:port authority: userinfo,
// the port, IPv6 brackets and a trailing dot are removed.
func Host(hostport string) string { return normalizeHost(hostport) }

// normalizeHost strips userinfo, the port, IPv6 brackets and a trailing dot,
// then lowercases, so only the host the transport dials remains.
func normalizeHost(hostport string) string {
	if i := strings.LastIndexByte(hostport, '@'); i >= 0 {
		hostport = hostport[i+1:]
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}
