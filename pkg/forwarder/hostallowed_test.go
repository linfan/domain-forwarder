package forwarder

import "testing"

func TestHostAllowed(t *testing.T) {
	whitelist := []string{"example.com", "API.Example.com", "127.0.0.1", "::1"}

	tests := []struct {
		host string
		want bool
	}{
		{"example.com", true},
		{"example.com:8080", true},
		{"EXAMPLE.com", true},
		{"example.com.", true},
		{"api.example.com", true},
		{"api.example.com:443", true},
		{"127.0.0.1:9090", true},
		{"[::1]:443", true},
		{"::1", true},
		{"www.example.com", false},
		{"example.com.evil.test", false},
		{"evil.test", false},
		{"example.com:8080@evil.test", false},
		{"", false},
	}

	for _, tc := range tests {
		if got := HostAllowed(whitelist, tc.host); got != tc.want {
			t.Errorf("HostAllowed(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}

	if !HostAllowed(nil, "anything.test") {
		t.Error("empty whitelist must allow every host")
	}
}

func TestHostBlocked(t *testing.T) {
	blacklist := []string{"blocked.example", "API.Blocked.example", "127.0.0.1", "::1"}

	tests := []struct {
		host string
		want bool
	}{
		{"blocked.example", true},
		{"blocked.example:8080", true},
		{"BLOCKED.example", true},
		{"blocked.example.", true},
		{"api.blocked.example:443", true},
		{"127.0.0.1:9090", true},
		{"[::1]:443", true},
		{"::1", true},
		{"allowed.example", false},
		{"sub.blocked.example", false},
		{"blocked.example.evil.test", false},
		{"", false},
	}

	for _, tc := range tests {
		if got := HostBlocked(blacklist, tc.host); got != tc.want {
			t.Errorf("HostBlocked(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}

	if HostBlocked(nil, "anything.test") {
		t.Error("empty blacklist must block nothing")
	}
}

func TestHostWildcard(t *testing.T) {
	whitelist := []string{"*.example.com"}

	tests := []struct {
		host string
		want bool
	}{
		{"api.example.com", true},
		{"API.Example.com:443", true},
		{"a.b.example.com", true},
		{"api.example.com.", true},
		{"example.com", false},
		{"example.com:443", false},
		{"notexample.com", false},
		{"example.com.evil.test", false},
		{"", false},
	}

	for _, tc := range tests {
		if got := HostAllowed(whitelist, tc.host); got != tc.want {
			t.Errorf("HostAllowed(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestHostCatchAll(t *testing.T) {
	for _, host := range []string{"anything.test", "example.com:8080", "127.0.0.1:9090", "[::1]:443"} {
		if !HostAllowed([]string{"*"}, host) {
			t.Errorf("HostAllowed(*, %q) = false, want true", host)
		}
		if !HostBlocked([]string{"*"}, host) {
			t.Errorf("HostBlocked(*, %q) = false, want true", host)
		}
	}

	if HostAllowed([]string{"*"}, "") {
		t.Error("catch-all whitelist must not admit an empty host")
	}
	if HostBlocked([]string{"*"}, "") {
		t.Error("catch-all blacklist must not match an empty host")
	}
}

func TestIPFilters(t *testing.T) {
	whitelist := []string{"10.0.0.1", "192.168.0.0/16", "2001:db8::/32"}
	blacklist := []string{"203.0.113.7", "198.51.100.0/24"}

	tests := []struct {
		ip      string
		allowed bool
		blocked bool
	}{
		{"10.0.0.1", true, false},
		{"192.168.1.20", true, false},
		{"2001:db8::1", true, false},
		{"10.0.0.2", false, false},
		{"203.0.113.7", false, true},
		{"198.51.100.9", false, true},
		{"203.0.113.8", false, false},
		{"not-an-ip", false, false},
	}

	for _, tc := range tests {
		if got := IPAllowed(whitelist, tc.ip); got != tc.allowed {
			t.Errorf("IPAllowed(%q) = %v, want %v", tc.ip, got, tc.allowed)
		}
		if got := IPBlocked(blacklist, tc.ip); got != tc.blocked {
			t.Errorf("IPBlocked(%q) = %v, want %v", tc.ip, got, tc.blocked)
		}
	}

	if !IPAllowed(nil, "10.0.0.2") {
		t.Error("empty whitelist must allow every IP")
	}
	if IPBlocked(nil, "10.0.0.2") {
		t.Error("empty blacklist must block nothing")
	}
}
