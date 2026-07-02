package storage

import (
	"testing"
	"time"
)

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

func TestMemoryManager_DeleteInteractions(t *testing.T) {
	idGen := func() string { return "test123" }
	manager := NewMemoryManager(idGen)

	// Create hook
	manager.CreateHook("example.com", CreateOptions{})

	// Add interactions
	int1 := DNSInteraction("int1", "1.2.3.4", "test.com", "A")
	int2 := DNSInteraction("int2", "1.2.3.4", "test.com", "A")
	int3 := DNSInteraction("int3", "1.2.3.4", "test.com", "A")

	manager.AddInteraction("test123", int1)
	manager.AddInteraction("test123", int2)
	manager.AddInteraction("test123", int3)

	// Delete specific interactions
	manager.DeleteInteractions("test123", []string{"int1", "int3"})

	// Poll remaining
	interactions := manager.PollInteractions("test123")
	if len(interactions) != 1 {
		t.Errorf("expected 1 interaction remaining, got %d", len(interactions))
	}

	if interactions[0].ID != "int2" {
		t.Errorf("expected remaining interaction to be int2, got %s", interactions[0].ID)
	}
}

func TestMemoryManager_DeleteHook(t *testing.T) {
	idGen := func() string { return "test123" }
	manager := NewMemoryManager(idGen)

	// Create hook
	manager.CreateHook("example.com", CreateOptions{})
	manager.AddInteraction("test123", DNSInteraction("int1", "1.2.3.4", "test.com", "A"))

	// Delete hook
	manager.DeleteHook("test123")

	// Verify hook is gone
	_, exists := manager.GetHook("test123")
	if exists {
		t.Error("expected hook to be deleted")
	}

	// Verify interactions are gone
	stats := manager.Stats()
	if stats.InteractionsTotal != 0 {
		t.Errorf("expected 0 interactions after hook deletion, got %d", stats.InteractionsTotal)
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

func TestMemoryManager_GetAllHooks(t *testing.T) {
	counter := 0
	idGen := func() string {
		counter++
		return "test-id-" + string(rune('0'+counter))
	}
	manager := NewMemoryManager(idGen)

	// Initially empty
	hooks := manager.GetAllHooks()
	if len(hooks) != 0 {
		t.Errorf("expected 0 hooks, got %d", len(hooks))
	}

	// Create multiple hooks
	hook1 := manager.CreateHook("example1.com", CreateOptions{})
	hook2 := manager.CreateHook("example2.com", CreateOptions{})
	hook3 := manager.CreateHook("example3.com", CreateOptions{})

	// Get all hooks
	hooks = manager.GetAllHooks()
	if len(hooks) != 3 {
		t.Errorf("expected 3 hooks, got %d", len(hooks))
	}

	// Verify all hooks are present
	hookIDs := make(map[string]bool)
	for _, h := range hooks {
		hookIDs[h.ID] = true
	}

	if !hookIDs[hook1.ID] || !hookIDs[hook2.ID] || !hookIDs[hook3.ID] {
		t.Error("not all created hooks were returned")
	}
}

func TestMemoryManager_GetAllInteractions(t *testing.T) {
	counter := 0
	idGen := func() string {
		counter++
		return "test-id-" + string(rune('0'+counter))
	}
	manager := NewMemoryManager(idGen)

	hook1 := manager.CreateHook("example1.com", CreateOptions{})
	hook2 := manager.CreateHook("example2.com", CreateOptions{})

	// Initially empty (but map has entries for the 2 hooks created above)
	allInteractions := manager.GetAllInteractions()

	totalCount := 0
	for _, interactions := range allInteractions {
		totalCount += len(interactions)
	}

	if totalCount != 0 {
		t.Errorf("expected 0 interactions, got %d", totalCount)
	}

	// Add interactions to different hooks
	int1 := DNSInteraction("int1", "1.2.3.4", "test1.com", "A")
	int2 := DNSInteraction("int2", "2.3.4.5", "test2.com", "A")
	int3 := HTTPInteraction("int3", "3.4.5.6", "GET", "/", map[string]string{}, "")
	int4 := HTTPInteraction("int4", "4.5.6.7", "POST", "/data", map[string]string{}, "body")

	manager.AddInteraction(hook1.ID, int1)
	manager.AddInteraction(hook1.ID, int2)
	manager.AddInteraction(hook2.ID, int3)
	manager.AddInteraction(hook2.ID, int4)

	// Get all interactions
	allInteractions = manager.GetAllInteractions()

	totalCount = 0
	interactionIDs := make(map[string]bool)

	for _, interactions := range allInteractions {
		totalCount += len(interactions)
		for _, i := range interactions {
			interactionIDs[i.ID] = true
		}
	}

	if totalCount != 4 {
		t.Errorf("expected 4 total interactions, got %d", totalCount)
	}

	if !interactionIDs["int1"] || !interactionIDs["int2"] || !interactionIDs["int3"] || !interactionIDs["int4"] {
		t.Error("not all interactions were returned")
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
