package credential

import (
	"strings"
	"testing"
)

func TestFingerprint(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"

	fp := Fingerprint(token)

	if strings.Contains(token, fp) || strings.Contains(fp, token) {
		t.Errorf("fingerprint %q must not disclose the token", fp)
	}
	if len(fp) != 8 {
		t.Errorf("expected an 8-char fingerprint, got %q", fp)
	}
	if fp != Fingerprint(token) || fp != HashFingerprint(Hash(token)) {
		t.Error("expected the fingerprint to be stable")
	}
	if fp == Fingerprint(token+"x") {
		t.Error("expected different tokens to yield different fingerprints")
	}
	if h := Hash(token); len(h) != 64 || strings.Contains(h, token) {
		t.Errorf("unexpected hash %q", h)
	}
}
