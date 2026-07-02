package storage

import (
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestSQLite(t *testing.T, maxBodyBytes int) *SQLiteManager {
	t.Helper()
	counter := 0
	idGen := func() string { counter++; return fmt.Sprintf("hook-%d", counter) }
	return newTestSQLiteWithID(t, maxBodyBytes, idGen)
}

func newTestSQLiteWithID(t *testing.T, maxBodyBytes int, idGen func() string) *SQLiteManager {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "longlived.db")
	m, err := NewSQLiteManager(path, idGen, maxBodyBytes, logger)
	if err != nil {
		t.Fatalf("NewSQLiteManager: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func TestSQLite_CreateAndGetHook(t *testing.T) {
	m := newTestSQLite(t, 1024)

	meta := map[string]any{"field": "profile.bio", "target": "acme"}
	created := m.CreateHook("example.com", CreateOptions{TTL: time.Hour, Metadata: meta})

	if created.DNS != "hook-1.example.com" {
		t.Errorf("unexpected DNS %q", created.DNS)
	}
	if created.ExpiresAt.IsZero() {
		t.Error("expected ExpiresAt to be set")
	}
	if !m.Has(created.ID) {
		t.Error("expected hook to be known")
	}
	if m.LongLivedCount() != 1 {
		t.Errorf("expected count 1, got %d", m.LongLivedCount())
	}

	got, ok := m.GetHook(created.ID)
	if !ok {
		t.Fatal("expected to retrieve hook")
	}
	if got.Metadata["field"] != "profile.bio" {
		t.Errorf("metadata not persisted: %v", got.Metadata)
	}
	// Timestamps round-trip through unix-nanos storage.
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("created_at mismatch: %s vs %s", got.CreatedAt, created.CreatedAt)
	}
}

func TestSQLite_AddAndPollInteractions(t *testing.T) {
	m := newTestSQLite(t, 1024)
	hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})

	m.AddInteraction(hook.ID, DNSInteraction("i1", "1.2.3.4", "q.example.com", "A"))
	m.AddInteraction(hook.ID, HTTPInteraction("i2", "5.6.7.8", "POST", "/x", map[string]string{"H": "v"}, "body"))

	got := m.PollInteractions(hook.ID)
	if len(got) != 2 {
		t.Fatalf("expected 2 interactions, got %d", len(got))
	}
	// Poll clears.
	if again := m.PollInteractions(hook.ID); len(again) != 0 {
		t.Errorf("expected poll to clear, got %d", len(again))
	}
}

func TestSQLite_UnknownHookIsDropped(t *testing.T) {
	m := newTestSQLite(t, 1024)

	m.AddInteraction("nope", DNSInteraction("i1", "1.2.3.4", "q", "A"))
	if _, ok := m.GetHook("nope"); ok {
		t.Error("expected unknown hook to be absent")
	}
	if got := m.PollInteractions("nope"); len(got) != 0 {
		t.Errorf("expected empty poll for unknown hook, got %d", len(got))
	}
}

func TestSQLite_BodyTruncation(t *testing.T) {
	const maxBody = 16
	m := newTestSQLite(t, maxBody)
	hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})

	longBody := strings.Repeat("x", maxBody*4)
	m.AddInteraction(hook.ID, HTTPInteraction("i1", "1.2.3.4", "POST", "/", nil, longBody))

	got := m.PollInteractions(hook.ID)
	if len(got) != 1 {
		t.Fatalf("expected 1 interaction, got %d", len(got))
	}
	body, _ := got[0].Data["body"].(string)
	if len(body) != maxBody {
		t.Errorf("expected body truncated to %d, got %d", maxBody, len(body))
	}
	if got[0].Data["truncated"] != true {
		t.Errorf("expected truncated flag set, got %v", got[0].Data["truncated"])
	}
}

func TestSQLite_PersistenceAcrossReopen(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "longlived.db")
	idGen := func() string { return "persist-1" }

	m1, err := NewSQLiteManager(path, idGen, 1024, logger)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	hook := m1.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	m1.AddInteraction(hook.ID, DNSInteraction("i1", "1.2.3.4", "q", "A"))
	m1.Close()

	// Reopen as if after a restart.
	m2, err := NewSQLiteManager(path, idGen, 1024, logger)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer m2.Close()

	if !m2.Has(hook.ID) {
		t.Fatal("expected hook to survive restart (index reloaded)")
	}
	// An interaction that arrives after the restart is captured, not dropped.
	m2.AddInteraction(hook.ID, DNSInteraction("i2", "9.9.9.9", "q2", "A"))

	got := m2.PollInteractions(hook.ID)
	if len(got) != 2 {
		t.Fatalf("expected 2 interactions after reopen, got %d", len(got))
	}
}

func TestSQLite_EvictExpiredHooks(t *testing.T) {
	m := newTestSQLite(t, 1024)

	expired := m.CreateHook("example.com", CreateOptions{TTL: time.Nanosecond})
	live := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	noExpiry := m.CreateHook("example.com", CreateOptions{})
	m.AddInteraction(expired.ID, DNSInteraction("i1", "1.2.3.4", "q", "A"))
	time.Sleep(time.Millisecond)

	n := m.EvictExpiredHooks(time.Now().UTC())
	if n != 1 {
		t.Errorf("expected 1 hook evicted, got %d", n)
	}
	if m.Has(expired.ID) {
		t.Error("expected expired hook removed from index")
	}
	if !m.Has(live.ID) || !m.Has(noExpiry.ID) {
		t.Error("expected live and no-expiry hooks to remain")
	}
	// Cascade removed its interactions.
	if got := m.PollInteractions(expired.ID); len(got) != 0 {
		t.Errorf("expected expired hook interactions gone, got %d", len(got))
	}
}

func TestSQLite_EnforcePerHookLimit(t *testing.T) {
	m := newTestSQLite(t, 1024)
	hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})

	base := time.Now().UTC()
	for i := 0; i < 10; i++ {
		it := DNSInteraction(fmt.Sprintf("i%d", i), "1.2.3.4", "q", "A")
		it.Timestamp = base.Add(time.Duration(i) * time.Second) // deterministic order
		m.AddInteraction(hook.ID, it)
	}

	removed := m.EnforcePerHookLimit(5)
	if removed != 5 {
		t.Errorf("expected 5 removed, got %d", removed)
	}

	got := m.PollInteractions(hook.ID)
	if len(got) != 5 {
		t.Fatalf("expected 5 remaining, got %d", len(got))
	}
	// The five newest survive (i5..i9); the oldest were dropped.
	for _, it := range got {
		if it.ID == "i0" || it.ID == "i4" {
			t.Errorf("expected oldest interactions dropped, found %s", it.ID)
		}
	}
}

func TestSQLite_LongLivedActivity(t *testing.T) {
	m := newTestSQLite(t, 1024)

	fired := m.CreateHook("example.com", CreateOptions{TTL: time.Hour, Metadata: map[string]any{"n": "1"}})
	quiet := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})

	m.AddInteraction(fired.ID, DNSInteraction("i1", "1.2.3.4", "q", "A"))
	m.AddInteraction(fired.ID, DNSInteraction("i2", "1.2.3.4", "q", "A"))

	activity := m.LongLivedActivity()
	if len(activity) != 1 {
		t.Fatalf("expected 1 active hook, got %d", len(activity))
	}
	if activity[0].Hook.ID != fired.ID {
		t.Errorf("expected fired hook, got %s", activity[0].Hook.ID)
	}
	if activity[0].PendingCount != 2 {
		t.Errorf("expected pending count 2, got %d", activity[0].PendingCount)
	}
	if activity[0].Hook.Metadata["n"] != "1" {
		t.Errorf("expected metadata in activity, got %v", activity[0].Hook.Metadata)
	}
	if activity[0].LastInteractionAt.IsZero() {
		t.Error("expected last interaction time to be set")
	}
	_ = quiet
}

func TestSQLite_EvictInteractionsBeforeIsNoOp(t *testing.T) {
	m := newTestSQLite(t, 1024)
	hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	m.AddInteraction(hook.ID, DNSInteraction("i1", "1.2.3.4", "q", "A"))

	// Even with a cutoff far in the future, long-lived interactions are retained.
	if n := m.EvictInteractionsBefore(time.Now().UTC().Add(time.Hour)); n != 0 {
		t.Errorf("expected no-op eviction, got %d", n)
	}
	if got := m.PollInteractions(hook.ID); len(got) != 1 {
		t.Errorf("expected interaction retained, got %d", len(got))
	}
}
