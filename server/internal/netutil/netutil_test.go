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

func TestHookIDFromHost(t *testing.T) {
	const domain = "hookd.example.com"

	tests := []struct {
		name     string
		host     string
		expected string
	}{
		{"simple subdomain", "abc123.hookd.example.com", "abc123"},
		{"multi-level subdomain", "deep.nested.abc123.hookd.example.com", "deep"},
		{"trailing dot", "abc123.hookd.example.com.", "abc123"},
		{"uppercase", "ABC123.HOOKD.EXAMPLE.COM", "abc123"},
		{"mixed case (dns-0x20)", "aBc123.HoOkD.eXaMpLe.CoM", "abc123"},
		{"apex is not a hook", "hookd.example.com", ""},
		{"foreign domain", "abc123.evil.com", ""},
		{"suffix without the dot", "nothookd.example.com", ""},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if result := HookIDFromHost(tt.host, domain); result != tt.expected {
				t.Errorf("HookIDFromHost(%q, %q) = %q, want %q", tt.host, domain, result, tt.expected)
			}
		})
	}
}

// The domain may itself be written with a trailing dot; it must match either way.
func TestHookIDFromHostTrailingDotDomain(t *testing.T) {
	if got := HookIDFromHost("abc123.hookd.example.com", "hookd.example.com."); got != "abc123" {
		t.Errorf("HookIDFromHost with dotted domain = %q, want %q", got, "abc123")
	}
}

func TestHookIDFromHostTail(t *testing.T) {
	for _, tc := range []struct {
		host string
		want string
	}{
		{"allowed.bucket.example.abc123.hookd.example.com", "abc123"},
		{"AbC123.HoOkD.ExAmPlE.CoM.", "abc123"},
		{"hookd.example.com", ""},
		{"abc123.evil.com", ""},
	} {
		if got := HookIDFromHostTail(tc.host, "hookd.example.com"); got != tc.want {
			t.Errorf("HookIDFromHostTail(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func TestResolveHookID(t *testing.T) {
	has := func(id string) bool { return id == "abc123" || id == "first" }
	for _, tc := range []struct {
		host string
		want string
	}{
		{"abc123.hookd.example.com", "abc123"},
		{"x.y.abc123.hookd.example.com", "abc123"},
		{"first.abc123.hookd.example.com", "first"},
		{"x.unknown.hookd.example.com", "x"},
		{"abc123.evil.com", ""},
	} {
		if got := ResolveHookID(tc.host, "hookd.example.com", has); got != tc.want {
			t.Errorf("ResolveHookID(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}
