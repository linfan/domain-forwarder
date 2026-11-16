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
		AccessControl: AccessControl{
			TargetDomain: AccessList{Whitelist: []string{"allowed.example"}},
		},
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
			w := serve(s, tc.method, tc.host, "192.0.2.1:1234")
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d (body %q)", w.Code, http.StatusForbidden, w.Body.String())
			}
		})
	}
}

func TestServeHTTPTargetDomainBlacklist(t *testing.T) {
	s := New(forwarder.New(forwarder.DefaultConfig()), Config{
		AccessControl: AccessControl{
			TargetDomain: AccessList{Blacklist: []string{"blocked.example"}},
		},
	})

	tests := []struct {
		name   string
		method string
		host   string
		want   int
	}{
		{"plain HTTP", http.MethodGet, "blocked.example", http.StatusForbidden},
		{"plain HTTP with port", http.MethodGet, "blocked.example:8080", http.StatusForbidden},
		{"CONNECT", http.MethodConnect, "blocked.example:443", http.StatusForbidden},
		{"unlisted host", http.MethodConnect, "127.0.0.1:1", http.StatusBadGateway},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := serve(s, tc.method, tc.host, "192.0.2.1:1234")
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %q)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestServeHTTPSourceIPWhitelist(t *testing.T) {
	s := New(forwarder.New(forwarder.DefaultConfig()), Config{
		AccessControl: AccessControl{
			SourceIP: AccessList{Whitelist: []string{"10.0.0.0/8"}},
		},
	})

	if w := serve(s, http.MethodConnect, "127.0.0.1:1", "192.0.2.1:1234"); w.Code != http.StatusForbidden {
		t.Fatalf("unlisted source status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if w := serve(s, http.MethodConnect, "127.0.0.1:1", "10.1.2.3:1234"); w.Code == http.StatusForbidden {
		t.Fatalf("whitelisted source must not be forbidden (body %q)", w.Body.String())
	}
}

func TestServeHTTPSourceIPBlacklist(t *testing.T) {
	s := New(forwarder.New(forwarder.DefaultConfig()), Config{
		AccessControl: AccessControl{
			SourceIP: AccessList{Blacklist: []string{"192.0.2.0/24"}},
		},
	})

	if w := serve(s, http.MethodConnect, "127.0.0.1:1", "192.0.2.1:1234"); w.Code != http.StatusForbidden {
		t.Fatalf("blacklisted source status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if w := serve(s, http.MethodConnect, "127.0.0.1:1", "198.51.100.7:1234"); w.Code == http.StatusForbidden {
		t.Fatalf("unlisted source must not be forbidden (body %q)", w.Body.String())
	}
}

// serve runs one request through the handler. Targets use port 1 so an allowed
// request fails fast with 502 instead of reaching a real service.
func serve(s *Server, method, host, remoteAddr string) *httptest.ResponseRecorder {
	r := &http.Request{
		Method:     method,
		URL:        &url.URL{Path: "/"},
		Host:       host,
		RemoteAddr: remoteAddr,
		Header:     make(http.Header),
		Body:       http.NoBody,
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
