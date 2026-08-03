package storage

import (
	"testing"
	"unicode/utf8"
)

func TestTruncateBody(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		max           int
		want          string
		wantTruncated bool
	}{
		{"under the cap", "hello", 10, "hello", false},
		{"exactly at the cap", "hello", 5, "hello", false},
		{"over the cap", "hello world", 5, "hello", true},
		{"no cap", "hello world", 0, "hello world", false},
		{"negative cap", "hello world", -1, "hello world", false},
		// The cut backs up to the rune start, dropping the 3-byte € entirely.
		{"cuts on a rune boundary", "ab€cd", 4, "ab", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, truncated := TruncateBody(tt.body, tt.max)
			if got != tt.want {
				t.Errorf("TruncateBody(%q, %d) = %q, want %q", tt.body, tt.max, got, tt.want)
			}
			if truncated != tt.wantTruncated {
				t.Errorf("expected truncated=%v, got %v", tt.wantTruncated, truncated)
			}
			if !utf8.ValidString(got) {
				t.Errorf("expected valid UTF-8, got %q", got)
			}
		})
	}
}
