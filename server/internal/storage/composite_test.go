package storage

import (
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

const ephemeralTTL = 24 * time.Hour

func newTestComposite(t *testing.T) (*CompositeManager, *SQLiteManager) {
	t.Helper()
	counter := 0
	idGen := func() string { counter++; return fmt.Sprintf("hook-%d", counter) }
	mem := NewMemoryManager(idGen)
	sqlite := newTestSQLiteWithID(t, 1024, idGen)
	return NewCompositeManager(mem, sqlite, ephemeralTTL), sqlite
}

func TestComposite_RoutesByTTL(t *testing.T) {
	c, sqlite := newTestComposite(t)

	// TTL at or below the threshold -> ephemeral (memory).
	eph := c.CreateHook("example.com", CreateOptions{TTL: ephemeralTTL})
	if sqlite.Has(eph.ID) {
		t.Error("ephemeral hook should not be in the long-lived store")
	}

	// TTL above the threshold -> long-lived (SQLite).
	ll := c.CreateHook("example.com", CreateOptions{TTL: ephemeralTTL + time.Hour})
	if !sqlite.Has(ll.ID) {
		t.Error("long-lived hook should be in the long-lived store")
	}
	if c.LongLivedCount() != 1 {
		t.Errorf("expected 1 long-lived hook, got %d", c.LongLivedCount())
	}

	// Both are retrievable through the composite.
	if _, ok := c.GetHook(eph.ID); !ok {
		t.Error("expected ephemeral hook retrievable")
	}
	if _, ok := c.GetHook(ll.ID); !ok {
		t.Error("expected long-lived hook retrievable")
	}
}

func TestComposite_InteractionRouting(t *testing.T) {
	c, sqlite := newTestComposite(t)

	eph := c.CreateHook("example.com", CreateOptions{TTL: ephemeralTTL})
	ll := c.CreateHook("example.com", CreateOptions{TTL: ephemeralTTL + time.Hour})

	c.AddInteraction(eph.ID, DNSInteraction("e1", "1.2.3.4", "q", "A"))
	c.AddInteraction(ll.ID, DNSInteraction("l1", "1.2.3.4", "q", "A"))

	// The long-lived interaction is persisted to SQLite.
	if got := sqlite.PollInteractions(ll.ID); len(got) != 1 {
		t.Errorf("expected long-lived interaction in sqlite, got %d", len(got))
	}
	// The ephemeral one is not.
	if sqlite.Has(eph.ID) {
		t.Error("ephemeral hook must not reach sqlite")
	}
	if got := c.PollInteractions(eph.ID); len(got) != 1 {
		t.Errorf("expected ephemeral interaction via composite, got %d", len(got))
	}
}

func TestComposite_StatsMerged(t *testing.T) {
	c, _ := newTestComposite(t)

	eph := c.CreateHook("example.com", CreateOptions{TTL: ephemeralTTL})
	ll := c.CreateHook("example.com", CreateOptions{TTL: ephemeralTTL + time.Hour})
	c.AddInteraction(eph.ID, DNSInteraction("e1", "1.2.3.4", "q", "A"))
	c.AddInteraction(ll.ID, HTTPInteraction("l1", "1.2.3.4", "GET", "/", nil, ""))

	stats := c.Stats()
	if stats.HooksActive != 2 {
		t.Errorf("expected 2 active hooks, got %d", stats.HooksActive)
	}
	if stats.InteractionsTotal != 2 {
		t.Errorf("expected 2 interactions, got %d", stats.InteractionsTotal)
	}
	if stats.InteractionsDNS != 1 || stats.InteractionsHTTP != 1 {
		t.Errorf("expected 1 dns + 1 http, got %d/%d", stats.InteractionsDNS, stats.InteractionsHTTP)
	}
}

func TestComposite_PollBatchMixed(t *testing.T) {
	c, _ := newTestComposite(t)

	eph := c.CreateHook("example.com", CreateOptions{TTL: ephemeralTTL})
	ll := c.CreateHook("example.com", CreateOptions{TTL: ephemeralTTL + time.Hour})
	c.AddInteraction(eph.ID, DNSInteraction("e1", "1.2.3.4", "q", "A"))
	c.AddInteraction(ll.ID, DNSInteraction("l1", "1.2.3.4", "q", "A"))

	res := c.PollInteractionsBatch([]string{eph.ID, ll.ID, "missing"})

	if got := res[eph.ID]; got == nil || len(got.Interactions) != 1 {
		t.Errorf("unexpected ephemeral batch result: %+v", got)
	}
	if got := res[ll.ID]; got == nil || len(got.Interactions) != 1 {
		t.Errorf("unexpected long-lived batch result: %+v", got)
	}
	if got := res["missing"]; got == nil || got.Error != "Hook not found" {
		t.Errorf("expected not-found for missing hook, got %+v", got)
	}
}

func TestComposite_EvictExpiredHooksBothStores(t *testing.T) {
	c, sqlite := newTestComposite(t)

	eph := c.CreateHook("example.com", CreateOptions{TTL: time.Nanosecond})         // memory, already expiring
	ll := c.CreateHook("example.com", CreateOptions{TTL: ephemeralTTL + time.Hour}) // sqlite, long-lived
	// Force the long-lived hook to be expired too.
	llExpired := c.CreateHook("example.com", CreateOptions{TTL: ephemeralTTL + time.Hour})
	sqlite.db.Exec(`UPDATE hooks SET expires_at = ? WHERE id = ?`, time.Now().Add(-time.Hour).UnixNano(), llExpired.ID)
	time.Sleep(2 * time.Millisecond)

	n := c.EvictExpiredHooks(time.Now().UTC())
	if n != 2 {
		t.Errorf("expected 2 hooks evicted across stores, got %d", n)
	}
	if _, ok := c.GetHook(eph.ID); ok {
		t.Error("expected expired ephemeral hook gone")
	}
	if _, ok := c.GetHook(llExpired.ID); ok {
		t.Error("expected expired long-lived hook gone")
	}
	if _, ok := c.GetHook(ll.ID); !ok {
		t.Error("expected live long-lived hook to remain")
	}
}

func TestComposite_NilLongLivedFallsBackToMemory(t *testing.T) {
	idGen := func() string { return "only" }
	mem := NewMemoryManager(idGen)
	c := NewCompositeManager(mem, nil, ephemeralTTL)

	// Even a long TTL routes to memory when the long-lived store is disabled.
	h := c.CreateHook("example.com", CreateOptions{TTL: ephemeralTTL + time.Hour})
	if _, ok := c.GetHook(h.ID); !ok {
		t.Error("expected hook retrievable from memory")
	}
	if c.LongLivedCount() != 0 {
		t.Errorf("expected 0 long-lived hooks, got %d", c.LongLivedCount())
	}
	if c.LongLivedActivity() != nil {
		t.Error("expected nil activity when long-lived disabled")
	}
	if err := c.Close(); err != nil {
		t.Errorf("expected Close to be a no-op, got %v", err)
	}
}

func TestComposite_ReloadAfterRestart(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "longlived.db")
	idGen := func() string { return "survivor" }

	sqlite1, err := NewSQLiteManager(path, idGen, 1024, logger)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	c1 := NewCompositeManager(NewMemoryManager(idGen), sqlite1, ephemeralTTL)
	h := c1.CreateHook("example.com", CreateOptions{TTL: ephemeralTTL + time.Hour})
	c1.Close()

	// New process: fresh memory store, reopened sqlite store.
	sqlite2, err := NewSQLiteManager(path, idGen, 1024, logger)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	c2 := NewCompositeManager(NewMemoryManager(idGen), sqlite2, ephemeralTTL)
	defer c2.Close()

	// The stored-XSS payload fires after the restart: it must be captured, not
	// silently dropped.
	c2.AddInteraction(h.ID, DNSInteraction("late", "9.9.9.9", "q", "A"))
	if got := c2.PollInteractions(h.ID); len(got) != 1 {
		t.Fatalf("expected post-restart interaction captured, got %d", len(got))
	}
}
