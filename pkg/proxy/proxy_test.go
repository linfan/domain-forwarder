package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"domain-forwarder/pkg/forwarder"
)

func TestServeHTTPTargetDomainWhitelist(t *testing.T) {
	s := New(forwarder.New(forwarder.DefaultConfig()), Config{
		TargetDomainWhitelist: []string{"allowed.example"},
	})

	tests := []struct {
		name   string
		method string
		host   string
	}{
		{"plain HTTP", http.MethodGet, "blocked.example"},
		{"plain HTTP with port", http.MethodGet, "blocked.example:8080"},
		{"CONNECT", http.MethodConnect, "blocked.example:443"},
		{"CONNECT IP literal", http.MethodConnect, "198.51.100.7:443"},
		{"spoofed userinfo", http.MethodGet, "allowed.example:80@blocked.example"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{
				Method:     tc.method,
				URL:        &url.URL{Path: "/"},
				Host:       tc.host,
				RemoteAddr: "192.0.2.1:1234",
				Header:     make(http.Header),
				Body:       http.NoBody,
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d (body %q)", w.Code, http.StatusForbidden, w.Body.String())
			}
		})
	}
}
