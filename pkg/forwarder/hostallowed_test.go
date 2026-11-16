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
