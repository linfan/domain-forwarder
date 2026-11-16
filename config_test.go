package main

import (
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

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
	if !slices.Contains(config.NextProtos, "h2") {
		t.Error("NextProtos must advertise h2 so force_attempt_http2 keeps working")
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
