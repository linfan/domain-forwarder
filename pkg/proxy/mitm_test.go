package proxy

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"domain-forwarder/pkg/forwarder"
	"domain-forwarder/pkg/mitm"
)

type recordedRequest struct {
	header     http.Header
	protoMajor int
}

func TestMITMEditsHTTP1(t *testing.T) {
	upstream, recorded := newTestUpstream(t, false)
	defer upstream.Close()

	proxyServer, caRoots := newTestMITMProxy(t, []string{"127.0.0.1"}, map[string]string{"X-Edit": "yes"}, nil, rootPool(upstream.Certificate()))
	defer proxyServer.Close()

	client := newTestMITMClient(t, proxyServer.URL, caRoots, false)
	defer client.CloseIdleConnections()

	resp, err := client.Get(upstream.URL + "/hello")
	if err != nil {
		t.Fatalf("GET through MITM proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 1 {
		t.Fatalf("response proto = %s, want HTTP/1.x", resp.Proto)
	}
	if body, _ := io.ReadAll(resp.Body); string(body) != "ok" {
		t.Fatalf("body = %q, want ok", body)
	}

	rec := waitForRequest(t, recorded)
	if got := rec.header.Get("X-Edit"); got != "yes" {
		t.Errorf("X-Edit = %q, want yes", got)
	}
	if got := rec.header.Get("X-Forwarded-Proto"); got != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want https", got)
	}
}

func TestMITMEditsHTTP2(t *testing.T) {
	upstream, recorded := newTestUpstream(t, true)
	defer upstream.Close()

	proxyServer, caRoots := newTestMITMProxy(t, []string{"127.0.0.1"}, map[string]string{"X-Edit": "yes"}, nil, rootPool(upstream.Certificate()))
	defer proxyServer.Close()

	client := newTestMITMClient(t, proxyServer.URL, caRoots, true)
	defer client.CloseIdleConnections()

	resp, err := client.Get(upstream.URL + "/hello")
	if err != nil {
		t.Fatalf("GET through MITM proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("response proto = %s, want HTTP/2", resp.Proto)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}

	rec := waitForRequest(t, recorded)
	if got := rec.header.Get("X-Edit"); got != "yes" {
		t.Errorf("X-Edit = %q, want yes", got)
	}
}

func TestMITMMirror(t *testing.T) {
	upstream, _ := newTestUpstream(t, false)
	defer upstream.Close()

	mirrored := make(chan http.Header, 1)
	mirrorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrored <- r.Header.Clone()
		_, _ = io.WriteString(w, "ok")
	}))
	defer mirrorServer.Close()

	target, err := url.Parse(mirrorServer.URL)
	if err != nil {
		t.Fatalf("parse mirror target: %v", err)
	}
	mirror := NewMirror(MirrorConfig{
		Target:       target,
		Timeout:      2 * time.Second,
		MaxBodyBytes: 1 << 20,
		QueueSize:    8,
	})

	proxyServer, caRoots := newTestMITMProxy(t, []string{"127.0.0.1"}, map[string]string{"X-Edit": "yes"}, mirror, rootPool(upstream.Certificate()))
	defer proxyServer.Close()

	// HTTP/2 is the interesting case for mirroring: bodyless GET requests still
	// carry a non-nil Body that never reaches EOF.
	client := newTestMITMClient(t, proxyServer.URL, caRoots, true)
	defer client.CloseIdleConnections()

	resp, err := client.Get(upstream.URL + "/hello")
	if err != nil {
		t.Fatalf("GET through MITM proxy: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	select {
	case header := <-mirrored:
		if got := header.Get("X-Edit"); got != "yes" {
			t.Errorf("mirrored X-Edit = %q, want yes", got)
		}
		if got := header.Get("X-Forwarded-Host"); got == "" {
			t.Error("mirrored request is missing X-Forwarded-Host")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mirror did not receive the decrypted request")
	}
}

func TestMITMBypassUnmatchedHost(t *testing.T) {
	upstream, recorded := newTestUpstream(t, false)
	defer upstream.Close()

	// The group does not cover 127.0.0.1, so the CONNECT stays an opaque
	// tunnel: the client must trust the upstream certificate itself.
	proxyServer, _ := newTestMITMProxy(t, []string{"example.com"}, map[string]string{"X-Edit": "yes"}, nil, rootPool(upstream.Certificate()))
	defer proxyServer.Close()

	client := newTestMITMClient(t, proxyServer.URL, rootPool(upstream.Certificate()), false)
	defer client.CloseIdleConnections()

	resp, err := client.Get(upstream.URL + "/hello")
	if err != nil {
		t.Fatalf("GET through raw tunnel: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}

	rec := waitForRequest(t, recorded)
	if got := rec.header.Get("X-Edit"); got != "" {
		t.Errorf("X-Edit = %q on a raw tunnel, want empty", got)
	}
}

func TestMITMRejectsProxyHTTP2CONNECT(t *testing.T) {
	_, certFile, keyFile := newTestCACertificate(t)
	authority, err := mitm.NewAuthority(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewAuthority: %v", err)
	}
	group, err := mitm.NewGroup("test", []string{"example.com"}, authority)
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	server := New(forwarder.New(forwarder.DefaultConfig()), Config{
		MITM: &mitm.Config{Groups: []*mitm.Group{group}},
	})

	req := httptest.NewRequest(http.MethodConnect, "https://example.com:443", nil)
	req.Host = "example.com:443"
	req.Proto = "HTTP/2.0"
	req.ProtoMajor = 2
	req.ProtoMinor = 0
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotImplemented {
		t.Fatalf("proxy-side HTTP/2 CONNECT status = %d, want 501", resp.Code)
	}
}

func TestMITMWebSocketUpgrade(t *testing.T) {
	upgraded := make(chan http.Header, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgraded <- r.Header.Clone()
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacking not supported", http.StatusInternalServerError)
			return
		}
		conn, brw, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		if err := brw.Flush(); err != nil {
			return
		}
		buf := make([]byte, 4)
		for {
			// Read through brw so bytes buffered with the handshake are echoed.
			if _, err := io.ReadFull(brw.Reader, buf); err != nil {
				return
			}
			if _, err := conn.Write(buf); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()

	proxyServer, caRoots := newTestMITMProxy(t, []string{"127.0.0.1"}, map[string]string{"X-Edit": "yes"}, nil, rootPool(upstream.Certificate()))
	defer proxyServer.Close()

	upstreamAddr := strings.TrimPrefix(upstream.URL, "https://")
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxyServer.URL, "http://"))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", upstreamAddr, upstreamAddr)
	connectResp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if connectResp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d", connectResp.StatusCode)
	}

	upstreamHost, _, err := net.SplitHostPort(upstreamAddr)
	if err != nil {
		t.Fatalf("split upstream address: %v", err)
	}
	tlsConn := tls.Client(conn, &tls.Config{RootCAs: caRoots, ServerName: upstreamHost})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake with MITM: %v", err)
	}
	defer tlsConn.Close()

	// Send the upgrade request and the first frame in one write so the frame is
	// buffered with the request, exercising the hijacked reader path.
	_, err = fmt.Fprintf(tlsConn, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\nping", upstreamAddr)
	if err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}
	reader := bufio.NewReader(tlsConn)
	upgradeResp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	if upgradeResp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d, want 101", upgradeResp.StatusCode)
	}

	echo := make([]byte, 4)
	if _, err := io.ReadFull(reader, echo); err != nil {
		t.Fatalf("read echoed bytes: %v", err)
	}
	if string(echo) != "ping" {
		t.Fatalf("echo = %q, want ping", echo)
	}

	select {
	case header := <-upgraded:
		if got := header.Get("X-Edit"); got != "yes" {
			t.Errorf("upgrade handshake X-Edit = %q, want yes", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not receive the upgrade handshake")
	}
}

func TestMITMPlaintextCONNECTFallsBack(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	defer upstream.Close()
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	// The catch-all group matches, but the tunnel carries plaintext, so the
	// proxy must fall back to a raw relay instead of failing the handshake.
	proxyServer, _ := newTestMITMProxy(t, []string{"*"}, map[string]string{"X-Edit": "yes"}, nil, x509.NewCertPool())
	defer proxyServer.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(proxyServer.URL, "http://"))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", upstream.Addr(), upstream.Addr())
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d", resp.StatusCode)
	}

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write tunnel data: %v", err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(reader, echo); err != nil {
		t.Fatalf("read echoed bytes: %v", err)
	}
	if string(echo) != "ping" {
		t.Fatalf("echo = %q, want ping", echo)
	}
}

func newTestUpstream(t *testing.T, http2 bool) (*httptest.Server, <-chan recordedRequest) {
	t.Helper()
	recorded := make(chan recordedRequest, 4)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorded <- recordedRequest{header: r.Header.Clone(), protoMajor: r.ProtoMajor}
		_, _ = io.WriteString(w, "ok")
	})
	if http2 {
		srv := httptest.NewUnstartedServer(handler)
		srv.EnableHTTP2 = true
		srv.StartTLS()
		return srv, recorded
	}
	return httptest.NewTLSServer(handler), recorded
}

// newTestMITMProxy starts a forward proxy that intercepts hosts using a
// freshly generated CA, and returns the proxy URL plus the CA root pool a
// client needs.
func newTestMITMProxy(t *testing.T, hosts []string, edits map[string]string, mirror *Mirror, upstreamRoots *x509.CertPool) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	caCert, caCertFile, caKeyFile := newTestCACertificate(t)
	authority, err := mitm.NewAuthority(caCertFile, caKeyFile)
	if err != nil {
		t.Fatalf("NewAuthority: %v", err)
	}
	group, err := mitm.NewGroup("test", hosts, authority)
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}

	engineConfig := forwarder.DefaultConfig()
	engineConfig.TLSClientConfig = &tls.Config{RootCAs: upstreamRoots}
	engine := forwarder.New(engineConfig)

	server := New(engine, Config{
		AddProxyHeaders:     true,
		EditHeaders:         edits,
		Mirror:              mirror,
		MITM:                &mitm.Config{Groups: []*mitm.Group{group}},
		DialTimeout:         2 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
		ReadHeaderTimeout:   5 * time.Second,
		IdleTimeout:         10 * time.Second,
		MaxHeaderBytes:      1 << 20,
		TLSClientConfig:     &tls.Config{RootCAs: upstreamRoots},
	})
	return httptest.NewServer(server), rootPool(caCert)
}

func newTestMITMClient(t *testing.T, proxyURL string, roots *x509.CertPool, http2 bool) *http.Client {
	t.Helper()
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:             http.ProxyURL(parsed),
		TLSClientConfig:   &tls.Config{RootCAs: roots},
		ForceAttemptHTTP2: http2,
	}}
}

func waitForRequest(t *testing.T, recorded <-chan recordedRequest) recordedRequest {
	t.Helper()
	select {
	case rec := <-recorded:
		return rec
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not receive a request")
		return recordedRequest{}
	}
}

func rootPool(cert *x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return pool
}

func newTestCACertificate(t *testing.T) (*x509.Certificate, string, string) {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate CA serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "domain-forwarder test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal CA key: %v", err)
	}
	certFile := filepath.Join(dir, "ca.crt")
	keyFile := filepath.Join(dir, "ca.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write CA certificate: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write CA key: %v", err)
	}
	return cert, certFile, keyFile
}
