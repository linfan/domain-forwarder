package main

import (
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMITMTunnelConnectValidation(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "ca.crt")
	keyFile := filepath.Join(dir, "ca.key")
	for _, name := range []string{certFile, keyFile} {
		if err := os.WriteFile(name, []byte("placeholder"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	mc := MITMConfig{
		Enabled: true,
		Groups: []MITMGroupConfig{{
			Name:       "all",
			CACertFile: certFile,
			CAKeyFile:  keyFile,
			Hosts:      []string{"*"},
		}},
	}
	if err := mc.Validate(); err != nil {
		t.Fatalf("mitm without exclusions must validate: %v", err)
	}

	mc.TunnelConnect = []string{"*:25", "*.example.com:25", "[::1]:143", "legacy.example.com"}
	if err := mc.Validate(); err != nil {
		t.Fatalf("valid tunnel_connect entries must validate: %v", err)
	}

	for _, bad := range []string{"*:https", "*:*", "example.com:0", "https://example.com:25", "example.com:"} {
		mc.TunnelConnect = []string{bad}
		err := mc.Validate()
		if err == nil {
			t.Fatalf("Validate accepted mitm.tunnel_connect %q", bad)
		}
		if !strings.Contains(err.Error(), "mitm.tunnel_connect") {
			t.Errorf("error for %q = %v, want it to name mitm.tunnel_connect", bad, err)
		}
	}
}

func TestUpstreamTLSConfigDisabledByDefault(t *testing.T) {
	fc := defaultForwardConfig()
	config, err := fc.UpstreamTLSConfig()
	if err != nil {
		t.Fatalf("UpstreamTLSConfig with no CA file: %v", err)
	}
	if config != nil {
		t.Fatalf("UpstreamTLSConfig = %+v, want nil so the system trust store is used", config)
	}
	if err := fc.Validate(); err != nil {
		t.Fatalf("default forward section must validate: %v", err)
	}
}

func TestUpstreamTLSConfigTrustsPrivateCA(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	dir := t.TempDir()
	caFile := filepath.Join(dir, "upstream-ca.pem")
	bundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw})
	if err := os.WriteFile(caFile, bundle, 0o600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}

	fc := defaultForwardConfig()
	fc.UpstreamCAFile = caFile
	if err := fc.Validate(); err != nil {
		t.Fatalf("Validate with a configured CA bundle: %v", err)
	}
	config, err := fc.UpstreamTLSConfig()
	if err != nil {
		t.Fatalf("UpstreamTLSConfig: %v", err)
	}
	if config.RootCAs == nil {
		t.Fatal("RootCAs must be set")
	}
	if config.NextProtos != nil {
		t.Errorf("NextProtos = %v, want nil so force_attempt_http2 alone decides ALPN", config.NextProtos)
	}

	// The pool must actually verify the private-CA upstream, which the system
	// trust store alone cannot do.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: config}}
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("request to a private-CA upstream: %v", err)
	}
	defer resp.Body.Close()
	if body, _ := io.ReadAll(resp.Body); string(body) != "ok" {
		t.Fatalf("body = %q, want ok", body)
	}
	if resp.TLS == nil || len(resp.TLS.VerifiedChains) == 0 {
		t.Fatal("upstream certificate was not verified against the configured pool")
	}
	if _, err := (&http.Client{}).Get(upstream.URL); err == nil {
		t.Error("system trust store alone must not verify the private-CA upstream")
	}
}

// TestUpstreamTLSConfigLeavesHTTP2ToTransport pins that a configured CA bundle
// does not also turn HTTP/2 on or off: an h2-capable upstream is reached over
// h2 only when force_attempt_http2 is set, never because the client config
// advertised ALPN by itself.
func TestUpstreamTLSConfigLeavesHTTP2ToTransport(t *testing.T) {
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()

	caFile := filepath.Join(t.TempDir(), "upstream-ca.pem")
	bundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw})
	if err := os.WriteFile(caFile, bundle, 0o600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}

	// A fresh config per case: http.Transport rewrites NextProtos on first use,
	// and reusing the mutated config would hide which switch enabled h2.
	for _, tc := range []struct {
		name       string
		forceHTTP2 bool
		wantProto  string
	}{
		{"force_attempt_http2 false", false, "HTTP/1.1"},
		{"force_attempt_http2 true", true, "HTTP/2.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := defaultForwardConfig()
			fc.UpstreamCAFile = caFile
			config, err := fc.UpstreamTLSConfig()
			if err != nil {
				t.Fatalf("UpstreamTLSConfig: %v", err)
			}
			client := &http.Client{Transport: &http.Transport{
				TLSClientConfig:   config,
				ForceAttemptHTTP2: tc.forceHTTP2,
			}}
			resp, err := client.Get(upstream.URL)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			if resp.Proto != tc.wantProto {
				t.Errorf("proto = %s, want %s", resp.Proto, tc.wantProto)
			}
		})
	}
}

func TestUpstreamTLSConfigRejectsUsableLookingFiles(t *testing.T) {
	fc := defaultForwardConfig()

	fc.UpstreamCAFile = filepath.Join(t.TempDir(), "missing.pem")
	if _, err := fc.UpstreamTLSConfig(); err == nil {
		t.Error("a missing CA file must be reported")
	} else if !strings.Contains(err.Error(), "upstream_ca_file") {
		t.Errorf("error should name the setting, got %v", err)
	}

	noCerts := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(noCerts, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	fc.UpstreamCAFile = noCerts
	if _, err := fc.UpstreamTLSConfig(); err == nil {
		t.Error("a bundle without certificates must be reported")
	}
	if err := fc.Validate(); err == nil {
		t.Error("Validate must reject a bundle without certificates at startup")
	}
}
