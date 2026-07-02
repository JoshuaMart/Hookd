package netutil

import "testing"

func TestExtractIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		expected   string
	}{
		{"ipv4 with port", "192.168.1.1:12345", "192.168.1.1"},
		{"ipv6 with port", "[::1]:8080", "::1"},
		{"no port", "192.168.1.1", "192.168.1.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if result := ExtractIP(tt.remoteAddr); result != tt.expected {
				t.Errorf("ExtractIP(%q) = %q, want %q", tt.remoteAddr, result, tt.expected)
			}
		})
	}
}
