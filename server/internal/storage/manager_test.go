package storage

import (
	"fmt"
	"testing"
	"time"
)

// TestMemoryManager_EvictByMemoryPressure_NoGCBelowThreshold proves that when
// heap usage is below the threshold, EvictByMemoryPressure does not force a GC
// and evicts nothing.
func TestMemoryManager_EvictByMemoryPressure_NoGCBelowThreshold(t *testing.T) {
	manager := NewMemoryManager(func() string { return "test-id" })

	gcCalls := 0
	manager.forceGC = func() { gcCalls++ }
	manager.heapInUseMB = func() int { return 10 } // below the 90 MB threshold

	hook := manager.CreateHook("example.com", CreateOptions{})
	manager.AddInteraction(hook.ID, DNSInteraction("int1", "1.2.3.4", "test.com", "A"))

	result := manager.EvictByMemoryPressure(100) // threshold = 90 MB

	if gcCalls != 0 {
		t.Errorf("expected no forced GC below threshold, got %d", gcCalls)
	}
	if result.InteractionsEvicted != 0 || result.HooksEvicted != 0 {
		t.Errorf("expected nothing evicted below threshold, got %+v", result)
	}
	if stats := manager.Stats(); stats.HooksActive != 1 {
		t.Errorf("expected hook to remain, got %d active", stats.HooksActive)
	}
}

// TestMemoryManager_EvictByMemoryPressure_StopsAtTarget proves the eviction loop
// stops once a fresh, post-GC reading drops below target, instead of evicting
// every hook.
func TestMemoryManager_EvictByMemoryPressure_StopsAtTarget(t *testing.T) {
	counter := 0
	manager := NewMemoryManager(func() string { counter++; return fmt.Sprintf("hook-%d", counter) })

	// Create 25 distinct hooks, each with one interaction.
	for i := 0; i < 25; i++ {
		h := manager.CreateHook("example.com", CreateOptions{})
		manager.AddInteraction(h.ID, DNSInteraction("int", "1.2.3.4", "test.com", "A"))
	}

	// Model heap usage as proportional to the number of active hooks: 4 MB per
	// hook. 25 hooks -> 100 MB (above threshold). It drops below target (80 MB)
	// once fewer than 20 hooks remain.
	manager.forceGC = func() {}
	manager.heapInUseMB = func() int { return manager.Stats().HooksActive * 4 }

	result := manager.EvictByMemoryPressure(100) // threshold = 90 MB, target = 80 MB

	active := manager.Stats().HooksActive
	if active == 0 {
		t.Fatal("expected eviction to stop before removing all hooks")
	}
	if active == 25 {
		t.Fatal("expected some hooks to be evicted under memory pressure")
	}
	// With batchSize=10 it evicts exactly one batch: 25 -> 15 remaining, and
	// 15*4=60 MB < 80 MB target stops the loop.
	if active != 15 {
		t.Errorf("expected 15 hooks remaining after one batch, got %d", active)
	}
	if result.HooksEvicted != 10 {
		t.Errorf("expected 10 hooks evicted, got %d", result.HooksEvicted)
	}
}

func TestMemoryManager_CreateHook_Options(t *testing.T) {
	manager := NewMemoryManager(func() string { return "test123" })

	t.Run("ttl sets expiry", func(t *testing.T) {
		before := time.Now().UTC()
		hook := manager.CreateHook("example.com", CreateOptions{TTL: time.Hour})
		if hook.ExpiresAt.IsZero() {
			t.Fatal("expected ExpiresAt to be set")
		}
		if hook.ExpiresAt.Before(before.Add(time.Hour)) {
			t.Errorf("expected ExpiresAt >= creation + 1h, got %s", hook.ExpiresAt)
		}
	})

	t.Run("zero ttl leaves expiry unset", func(t *testing.T) {
		hook := manager.CreateHook("example.com", CreateOptions{})
		if !hook.ExpiresAt.IsZero() {
			t.Errorf("expected zero ExpiresAt for zero TTL, got %s", hook.ExpiresAt)
		}
	})

	t.Run("metadata is preserved", func(t *testing.T) {
		meta := map[string]any{"field": "profile.bio", "target": "acme"}
		hook := manager.CreateHook("example.com", CreateOptions{Metadata: meta})
		if hook.Metadata["field"] != "profile.bio" {
			t.Errorf("expected metadata to be preserved, got %v", hook.Metadata)
		}
	})
}

func TestMemoryManager_CreateHook(t *testing.T) {
	idGen := func() string { return "test123" }
	manager := NewMemoryManager(idGen)

	hook := manager.CreateHook("example.com", CreateOptions{})

	if hook.ID != "test123" {
		t.Errorf("expected ID test123, got %s", hook.ID)
	}

	if hook.DNS != "test123.example.com" {
		t.Errorf("expected DNS test123.example.com, got %s", hook.DNS)
	}

	if hook.HTTP != "http://test123.example.com" {
		t.Errorf("expected HTTP http://test123.example.com, got %s", hook.HTTP)
	}
}

func TestMemoryManager_GetHook(t *testing.T) {
	idGen := func() string { return "test123" }
	manager := NewMemoryManager(idGen)

	// Create hook
	created := manager.CreateHook("example.com", CreateOptions{})

	// Retrieve hook
	retrieved, exists := manager.GetHook("test123")

	if !exists {
		t.Error("expected hook to exist")
	}

	if retrieved.ID != created.ID {
		t.Errorf("expected ID %s, got %s", created.ID, retrieved.ID)
	}

	// Try non-existent hook
	_, exists = manager.GetHook("nonexistent")
	if exists {
		t.Error("expected hook not to exist")
	}
}

func TestMemoryManager_AddInteraction(t *testing.T) {
	idGen := func() string { return "test123" }
	manager := NewMemoryManager(idGen)

	// Create hook
	manager.CreateHook("example.com", CreateOptions{})

	// Add interaction
	interaction := DNSInteraction("int1", "1.2.3.4", "test.com", "A")
	manager.AddInteraction("test123", interaction)

	// Verify stats
	stats := manager.Stats()
	if stats.InteractionsTotal != 1 {
		t.Errorf("expected 1 interaction, got %d", stats.InteractionsTotal)
	}

	if stats.InteractionsDNS != 1 {
		t.Errorf("expected 1 DNS interaction, got %d", stats.InteractionsDNS)
	}
}

func TestMemoryManager_PollInteractions(t *testing.T) {
	idGen := func() string { return "test123" }
	manager := NewMemoryManager(idGen)

	// Create hook
	manager.CreateHook("example.com", CreateOptions{})

	// Add interactions
	int1 := DNSInteraction("int1", "1.2.3.4", "test.com", "A")
	int2 := HTTPInteraction("int2", "5.6.7.8", "POST", "/callback", map[string]string{"User-Agent": "curl"}, "body")

	manager.AddInteraction("test123", int1)
	manager.AddInteraction("test123", int2)

	// Poll interactions
	interactions := manager.PollInteractions("test123")
	if len(interactions) != 2 {
		t.Errorf("expected 2 interactions, got %d", len(interactions))
	}

	// Verify interactions are cleared
	interactions = manager.PollInteractions("test123")
	if len(interactions) != 0 {
		t.Errorf("expected 0 interactions after poll, got %d", len(interactions))
	}
}

func TestMemoryManager_Stats(t *testing.T) {
	idGen := func() string { return "test123" }
	manager := NewMemoryManager(idGen)

	// Create hook
	manager.CreateHook("example.com", CreateOptions{})

	// Add mixed interactions
	manager.AddInteraction("test123", DNSInteraction("int1", "1.2.3.4", "test.com", "A"))
	manager.AddInteraction("test123", DNSInteraction("int2", "1.2.3.4", "test.com", "A"))
	manager.AddInteraction("test123", HTTPInteraction("int3", "5.6.7.8", "GET", "/", map[string]string{}, ""))

	stats := manager.Stats()

	if stats.HooksActive != 1 {
		t.Errorf("expected 1 active hook, got %d", stats.HooksActive)
	}

	if stats.InteractionsTotal != 3 {
		t.Errorf("expected 3 total interactions, got %d", stats.InteractionsTotal)
	}

	if stats.InteractionsDNS != 2 {
		t.Errorf("expected 2 DNS interactions, got %d", stats.InteractionsDNS)
	}

	if stats.InteractionsHTTP != 1 {
		t.Errorf("expected 1 HTTP interaction, got %d", stats.InteractionsHTTP)
	}
}

func TestMemoryManager_AddInteraction_NonExistentHook(t *testing.T) {
	manager := NewMemoryManager(func() string { return "test-id" })

	interaction := DNSInteraction("int1", "1.2.3.4", "test.com", "A")

	// AddInteraction silently ignores non-existent hooks.
	manager.AddInteraction("nonexistent", interaction)

	// Verify interaction was not added
	stats := manager.Stats()
	if stats.InteractionsTotal != 0 {
		t.Errorf("expected 0 interactions, got %d", stats.InteractionsTotal)
	}
}

func TestMemoryManager_PollInteractions_NonExistentHook(t *testing.T) {
	manager := NewMemoryManager(func() string { return "test-id" })

	// PollInteractions returns an empty slice for a non-existent hook.
	interactions := manager.PollInteractions("nonexistent")

	if len(interactions) != 0 {
		t.Errorf("expected 0 interactions for non-existent hook, got %d", len(interactions))
	}
}

func TestMemoryManager_PollInteractionsBatch(t *testing.T) {
	counter := 0
	manager := NewMemoryManager(func() string { counter++; return fmt.Sprintf("hook-%d", counter) })

	withData := manager.CreateHook("example.com", CreateOptions{})
	empty := manager.CreateHook("example.com", CreateOptions{})
	manager.AddInteraction(withData.ID, DNSInteraction("i1", "1.2.3.4", "q", "A"))
	manager.AddInteraction(withData.ID, HTTPInteraction("i2", "5.6.7.8", "GET", "/", nil, ""))

	res := manager.PollInteractionsBatch([]string{withData.ID, empty.ID, "missing"})

	if got := res[withData.ID]; got == nil || len(got.Interactions) != 2 {
		t.Errorf("expected 2 interactions for populated hook, got %+v", got)
	}
	if got := res[empty.ID]; got == nil || got.Error != "" || len(got.Interactions) != 0 {
		t.Errorf("expected empty (non-nil) result for empty hook, got %+v", got)
	}
	if got := res["missing"]; got == nil || got.Error != "Hook not found" {
		t.Errorf("expected not-found for missing hook, got %+v", got)
	}

	// The batch poll clears interactions, just like the single poll.
	if again := manager.PollInteractions(withData.ID); len(again) != 0 {
		t.Errorf("expected batch poll to clear interactions, got %d", len(again))
	}
}

func TestMemoryManager_EvictInteractionsBefore(t *testing.T) {
	counter := 0
	manager := NewMemoryManager(func() string { counter++; return fmt.Sprintf("hook-%d", counter) })

	hook := manager.CreateHook("example.com", CreateOptions{})
	cutoff := time.Now().UTC()

	old1 := DNSInteraction("old1", "1.2.3.4", "q", "A")
	old1.Timestamp = cutoff.Add(-2 * time.Hour)
	old2 := DNSInteraction("old2", "1.2.3.4", "q", "A")
	old2.Timestamp = cutoff.Add(-time.Hour)
	fresh := DNSInteraction("fresh", "1.2.3.4", "q", "A")
	fresh.Timestamp = cutoff.Add(time.Hour)
	manager.AddInteraction(hook.ID, old1)
	manager.AddInteraction(hook.ID, old2)
	manager.AddInteraction(hook.ID, fresh)

	removed := manager.EvictInteractionsBefore(cutoff)
	if removed != 2 {
		t.Errorf("expected 2 interactions evicted, got %d", removed)
	}

	got := manager.PollInteractions(hook.ID)
	if len(got) != 1 || got[0].ID != "fresh" {
		t.Errorf("expected only the fresh interaction to remain, got %+v", got)
	}

	// A second pass with nothing older than the cutoff removes nothing.
	if n := manager.EvictInteractionsBefore(cutoff); n != 0 {
		t.Errorf("expected no further eviction, got %d", n)
	}
}

func TestMemoryManager_EnforcePerHookLimit(t *testing.T) {
	counter := 0
	manager := NewMemoryManager(func() string { counter++; return fmt.Sprintf("hook-%d", counter) })

	over := manager.CreateHook("example.com", CreateOptions{})
	under := manager.CreateHook("example.com", CreateOptions{})

	for i := 0; i < 7; i++ {
		manager.AddInteraction(over.ID, DNSInteraction(fmt.Sprintf("o%d", i), "1.2.3.4", "q", "A"))
	}
	for i := 0; i < 2; i++ {
		manager.AddInteraction(under.ID, DNSInteraction(fmt.Sprintf("u%d", i), "1.2.3.4", "q", "A"))
	}

	removed := manager.EnforcePerHookLimit(3)
	if removed != 4 {
		t.Errorf("expected 4 interactions dropped from over-limit hook, got %d", removed)
	}

	got := manager.PollInteractions(over.ID)
	if len(got) != 3 {
		t.Fatalf("expected 3 interactions retained, got %d", len(got))
	}
	// The newest survive; the oldest (o0..o3) were dropped.
	for _, it := range got {
		if it.ID == "o0" || it.ID == "o3" {
			t.Errorf("expected oldest interactions dropped, found %s", it.ID)
		}
	}
	// The under-limit hook is untouched.
	if got := manager.PollInteractions(under.ID); len(got) != 2 {
		t.Errorf("expected under-limit hook untouched, got %d", len(got))
	}
}
