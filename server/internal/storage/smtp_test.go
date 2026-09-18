package storage

import (
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewHookSMTPAddress(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		hook := newHook("abc123", "hookd.example.com", CreateOptions{SMTPEnabled: true})
		if hook.SMTP != "abc123@hookd.example.com" {
			t.Errorf("SMTP = %q, want %q", hook.SMTP, "abc123@hookd.example.com")
		}
	})

	// Without a listener the address would black-hole, so it is not advertised.
	t.Run("disabled", func(t *testing.T) {
		hook := newHook("abc123", "hookd.example.com", CreateOptions{})
		if hook.SMTP != "" {
			t.Errorf("SMTP = %q, want it empty when SMTP is disabled", hook.SMTP)
		}
	})
}

func TestSMTPInteractionFields(t *testing.T) {
	it := SMTPInteraction("i1", "203.0.113.5", "sender.test", "signup@vendor.test",
		"abc123+tag@hookd.example.com", "tag", "Verify", "raw message")

	if it.Type != InteractionTypeSMTP {
		t.Errorf("type = %q, want %q", it.Type, InteractionTypeSMTP)
	}

	want := map[string]string{
		"helo":      "sender.test",
		"mail_from": "signup@vendor.test",
		"rcpt_to":   "abc123+tag@hookd.example.com",
		"tag":       "tag",
		"subject":   "Verify",
		// The raw message lives under "body" so the long-lived store truncates
		// it the same way it truncates an HTTP body.
		"body": "raw message",
	}
	for key, expected := range want {
		if got, _ := it.Data[key].(string); got != expected {
			t.Errorf("data[%q] = %q, want %q", key, got, expected)
		}
	}
}

func TestMemoryManagerCountsSMTPInteractions(t *testing.T) {
	m := NewMemoryManager(func() string { return "hook-1" })
	hook := m.CreateHook("example.com", CreateOptions{SMTPEnabled: true})

	m.AddInteraction(hook.ID, SMTPInteraction("i1", "1.2.3.4", "h", "f", "r", "", "s", "b"))
	m.AddInteraction(hook.ID, HTTPInteraction("i2", "1.2.3.4", "GET", "/", nil, ""))

	stats := m.Stats()
	if stats.InteractionsSMTP != 1 {
		t.Errorf("InteractionsSMTP = %d, want 1", stats.InteractionsSMTP)
	}
	if stats.InteractionsTotal != 2 {
		t.Errorf("InteractionsTotal = %d, want 2", stats.InteractionsTotal)
	}
}

func TestSQLite_SMTPRoundTripAndTruncation(t *testing.T) {
	m := newTestSQLite(t, 32)
	hook := m.CreateHook("hookd.example.com", CreateOptions{TTL: time.Hour, SMTPEnabled: true})

	if hook.SMTP != hook.ID+"@hookd.example.com" {
		t.Fatalf("SMTP = %q, want %q", hook.SMTP, hook.ID+"@hookd.example.com")
	}

	// The address survives the column round-trip.
	got, ok := m.GetHook(hook.ID)
	if !ok {
		t.Fatal("expected to retrieve the hook")
	}
	if got.SMTP != hook.SMTP {
		t.Errorf("persisted SMTP = %q, want %q", got.SMTP, hook.SMTP)
	}

	raw := "Subject: long\r\n\r\n" + strings.Repeat("y", 500)
	m.AddInteraction(hook.ID, SMTPInteraction("i1", "1.2.3.4", "h", "f", "r", "", "long", raw))

	interactions := m.PollInteractions(hook.ID)
	if len(interactions) != 1 {
		t.Fatalf("got %d interactions, want 1", len(interactions))
	}
	body, _ := interactions[0].Data["body"].(string)
	if len(body) > 32 {
		t.Errorf("body is %d bytes, want it truncated to 32", len(body))
	}
	if truncated, _ := interactions[0].Data["truncated"].(bool); !truncated {
		t.Error("expected the interaction to be flagged as truncated")
	}

	if stats := m.Stats(); stats.InteractionsSMTP != 0 {
		// Polled interactions are deleted, so nothing should remain counted.
		t.Errorf("InteractionsSMTP = %d after polling, want 0", stats.InteractionsSMTP)
	}
}

// A database created before the smtp column existed must gain it on open,
// rather than diverging from a fresh install.
func TestSQLite_MigratesLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	legacy, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE hooks (
			id         TEXT PRIMARY KEY,
			dns        TEXT NOT NULL,
			http       TEXT NOT NULL,
			https      TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			metadata   TEXT
		);
		INSERT INTO hooks VALUES ('old1', 'old1.example.com', 'http://old1.example.com', 'https://old1.example.com', 1, 0, NULL);
	`); err != nil {
		t.Fatalf("seed legacy db: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	counter := 0
	idGen := func() string { counter++; return fmt.Sprintf("new-%d", counter) }
	m, err := NewSQLiteManager(path, idGen, 1024, logger)
	if err != nil {
		t.Fatalf("NewSQLiteManager on a legacy db: %v", err)
	}
	defer func() { _ = m.Close() }()

	// The pre-existing hook is readable, with an empty address.
	old, ok := m.GetHook("old1")
	if !ok {
		t.Fatal("expected the legacy hook to survive the migration")
	}
	if old.SMTP != "" {
		t.Errorf("legacy SMTP = %q, want it empty (not backfilled)", old.SMTP)
	}

	// A hook registered after the migration gets one.
	fresh := m.CreateHook("hookd.example.com", CreateOptions{TTL: time.Hour, SMTPEnabled: true})
	if fresh.SMTP == "" {
		t.Error("expected a hook created after the migration to carry an SMTP address")
	}
	if got, _ := m.GetHook(fresh.ID); got.SMTP != fresh.SMTP {
		t.Errorf("persisted SMTP = %q, want %q", got.SMTP, fresh.SMTP)
	}
}

// Opening the same database twice must not fail on the already-applied step.
func TestSQLite_MigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "longlived.db")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	idGen := func() string { return "hook-1" }

	for i := 0; i < 2; i++ {
		m, err := NewSQLiteManager(path, idGen, 1024, logger)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if err := m.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
}
