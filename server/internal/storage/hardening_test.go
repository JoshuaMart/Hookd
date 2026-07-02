package storage

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSQLite_CreateLongLivedHook_CapAtomic verifies the cap is enforced by the
// store itself (the invariant the API's pre-check cannot guarantee under races).
func TestSQLite_CreateLongLivedHook_CapAtomic(t *testing.T) {
	m := newTestSQLite(t, 1024)

	for i := 0; i < 2; i++ {
		if _, err := m.CreateLongLivedHook("example.com", CreateOptions{TTL: time.Hour}, 2); err != nil {
			t.Fatalf("create %d: unexpected error %v", i, err)
		}
	}
	// Third create must be refused.
	if _, err := m.CreateLongLivedHook("example.com", CreateOptions{TTL: time.Hour}, 2); err != ErrHookLimitReached {
		t.Errorf("expected ErrHookLimitReached, got %v", err)
	}
	if m.LongLivedCount() != 2 {
		t.Errorf("expected count to stay at cap (2), got %d", m.LongLivedCount())
	}
}

// TestSQLite_CreateHook_MkdirParent proves NewSQLiteManager creates a missing
// parent directory instead of failing to open (the fresh-host boot case).
func TestSQLite_CreateHook_MkdirParent(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Nested path whose parent directories do not exist yet.
	path := filepath.Join(t.TempDir(), "nested", "deeper", "longlived.db")
	m, err := NewSQLiteManager(path, func() string { return "id-1" }, 1024, logger)
	if err != nil {
		t.Fatalf("expected NewSQLiteManager to create the parent dir, got %v", err)
	}
	defer m.Close()
	if _, err := m.CreateLongLivedHook("example.com", CreateOptions{TTL: time.Hour}, 0); err != nil {
		t.Errorf("create after mkdir: %v", err)
	}
}

// TestSQLite_TruncateBody_RuneSafe proves truncation never splits a multibyte
// rune (which would corrupt the stored body with U+FFFD).
func TestSQLite_TruncateBody_RuneSafe(t *testing.T) {
	// maxBody lands one byte into a 3-byte '€' if we cut naively.
	const maxBody = 65
	m := newTestSQLite(t, maxBody)
	hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})

	body := strings.Repeat("a", 64) + "€" + strings.Repeat("b", 100) // '€' starts at byte 64
	m.AddInteraction(hook.ID, HTTPInteraction("i1", "1.2.3.4", "POST", "/", nil, body))

	got := m.PollInteractions(hook.ID)
	if len(got) != 1 {
		t.Fatalf("expected 1 interaction, got %d", len(got))
	}
	stored, _ := got[0].Data["body"].(string)
	if strings.ContainsRune(stored, '�') {
		t.Errorf("stored body contains a corrupted replacement rune: %q", stored)
	}
	// Cut on the rune boundary before '€' -> exactly 64 'a's retained.
	if stored != strings.Repeat("a", 64) {
		t.Errorf("expected clean truncation to 64 bytes, got %d bytes %q", len(stored), stored)
	}
	if got[0].Data["truncated"] != true {
		t.Error("expected truncated flag")
	}
}
