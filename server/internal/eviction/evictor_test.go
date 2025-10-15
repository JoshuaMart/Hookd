package eviction

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/storage"
)

func TestEvictor_EvictByTTL(t *testing.T) {
	idGen := func() string { return "test123" }
	manager := storage.NewMemoryManager(idGen)
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := config.EvictionConfig{
		InteractionTTL:  100 * time.Millisecond,
		MaxPerHook:      1000,
		MaxMemoryMB:     1800,
		CleanupInterval: 50 * time.Millisecond,
	}

	evictor := NewEvictor(manager, cfg, logger)

	// Create hook and add interaction
	manager.CreateHook("example.com")
	manager.AddInteraction("test123", storage.DNSInteraction("int1", "1.2.3.4", "test.com", "A"))

	// Wait for TTL to expire
	time.Sleep(150 * time.Millisecond)

	// Run eviction
	evictor.evictByTTL()

	// Check that interaction was evicted
	stats := manager.Stats()
	if stats.InteractionsTotal != 0 {
		t.Errorf("expected 0 interactions after TTL eviction, got %d", stats.InteractionsTotal)
	}

	if evictor.metrics.EvictionsTTL != 1 {
		t.Errorf("expected 1 TTL eviction, got %d", evictor.metrics.EvictionsTTL)
	}
}

func TestEvictor_EvictByLimit(t *testing.T) {
	idGen := func() string { return "test123" }
	manager := storage.NewMemoryManager(idGen)
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := config.EvictionConfig{
		InteractionTTL:  1 * time.Hour,
		MaxPerHook:      5,
		MaxMemoryMB:     1800,
		CleanupInterval: 10 * time.Second,
	}

	evictor := NewEvictor(manager, cfg, logger)

	// Create hook
	manager.CreateHook("example.com")

	// Add more interactions than the limit
	for i := 0; i < 10; i++ {
		manager.AddInteraction("test123", storage.DNSInteraction("int"+string(rune(i)), "1.2.3.4", "test.com", "A"))
	}

	// Run eviction
	evictor.evictByLimit()

	// Check that oldest interactions were evicted
	stats := manager.Stats()
	if stats.InteractionsTotal != 5 {
		t.Errorf("expected 5 interactions after limit eviction, got %d", stats.InteractionsTotal)
	}

	if evictor.metrics.EvictionsLimit != 5 {
		t.Errorf("expected 5 limit evictions, got %d", evictor.metrics.EvictionsLimit)
	}
}

func TestEvictor_Start(t *testing.T) {
	idGen := func() string { return "test123" }
	manager := storage.NewMemoryManager(idGen)
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := config.EvictionConfig{
		InteractionTTL:  50 * time.Millisecond,
		MaxPerHook:      1000,
		MaxMemoryMB:     1800,
		CleanupInterval: 30 * time.Millisecond,
	}

	evictor := NewEvictor(manager, cfg, logger)

	// Create hook and add interaction
	manager.CreateHook("example.com")
	manager.AddInteraction("test123", storage.DNSInteraction("int1", "1.2.3.4", "test.com", "A"))

	// Start evictor in background
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go evictor.Start(ctx)

	// Wait for eviction to occur
	time.Sleep(100 * time.Millisecond)

	// Check that interaction was evicted
	stats := manager.Stats()
	if stats.InteractionsTotal != 0 {
		t.Errorf("expected 0 interactions after automatic eviction, got %d", stats.InteractionsTotal)
	}
}

func TestEvictor_GetMetrics(t *testing.T) {
	idGen := func() string { return "test123" }
	manager := storage.NewMemoryManager(idGen)
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := config.EvictionConfig{
		InteractionTTL:  1 * time.Hour,
		MaxPerHook:      1000,
		MaxMemoryMB:     1800,
		CleanupInterval: 10 * time.Second,
	}

	evictor := NewEvictor(manager, cfg, logger)

	metrics := evictor.GetMetrics()

	if metrics.EvictionsTTL != 0 {
		t.Errorf("expected 0 TTL evictions, got %d", metrics.EvictionsTTL)
	}

	if metrics.EvictionsLimit != 0 {
		t.Errorf("expected 0 limit evictions, got %d", metrics.EvictionsLimit)
	}

	if metrics.EvictionsMemory != 0 {
		t.Errorf("expected 0 memory evictions, got %d", metrics.EvictionsMemory)
	}
}

func TestEvictor_EvictByMemory(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	// Set a very low memory limit to trigger eviction
	cfg := config.EvictionConfig{
		InteractionTTL:  1 * time.Hour,
		MaxPerHook:      1000,
		MaxMemoryMB:     1, // Very low limit
		CleanupInterval: 10 * time.Second,
	}

	evictor := NewEvictor(manager, cfg, logger)

	// Create multiple hooks with interactions
	hook1 := manager.CreateHook("example1.com")
	hook2 := manager.CreateHook("example2.com")
	hook3 := manager.CreateHook("example3.com")

	// Add interactions to each hook
	for i := 0; i < 5; i++ {
		manager.AddInteraction(hook1.ID, storage.DNSInteraction("h1-int"+string(rune(i)), "1.2.3.4", "test.com", "A"))
		manager.AddInteraction(hook2.ID, storage.DNSInteraction("h2-int"+string(rune(i)), "2.3.4.5", "test.com", "A"))
		manager.AddInteraction(hook3.ID, storage.DNSInteraction("h3-int"+string(rune(i)), "3.4.5.6", "test.com", "A"))
	}

	// Wait a bit to ensure different creation times
	time.Sleep(10 * time.Millisecond)

	// Manually trigger memory eviction
	evictor.evictByMemory()

	// Check that some hooks were evicted
	stats := manager.Stats()
	if stats.HooksActive == 3 {
		t.Error("expected some hooks to be evicted due to memory pressure")
	}

	// Memory evictions should have been recorded
	if evictor.metrics.EvictionsMemory == 0 {
		t.Error("expected memory evictions to be recorded")
	}
}

func TestEvictor_EvictByMemory_BelowThreshold(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	// Set a high memory limit to NOT trigger eviction
	cfg := config.EvictionConfig{
		InteractionTTL:  1 * time.Hour,
		MaxPerHook:      1000,
		MaxMemoryMB:     10000, // Very high limit
		CleanupInterval: 10 * time.Second,
	}

	evictor := NewEvictor(manager, cfg, logger)

	// Create hook with interactions
	hook := manager.CreateHook("example.com")
	manager.AddInteraction(hook.ID, storage.DNSInteraction("int1", "1.2.3.4", "test.com", "A"))

	// Manually trigger memory eviction
	evictor.evictByMemory()

	// Check that nothing was evicted
	stats := manager.Stats()
	if stats.HooksActive != 1 {
		t.Errorf("expected 1 hook, got %d", stats.HooksActive)
	}

	if stats.InteractionsTotal != 1 {
		t.Errorf("expected 1 interaction, got %d", stats.InteractionsTotal)
	}

	if evictor.metrics.EvictionsMemory != 0 {
		t.Errorf("expected 0 memory evictions, got %d", evictor.metrics.EvictionsMemory)
	}
}

func TestEvictor_EvictByMemory_NoHooks(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := config.EvictionConfig{
		InteractionTTL:  1 * time.Hour,
		MaxPerHook:      1000,
		MaxMemoryMB:     1, // Very low limit
		CleanupInterval: 10 * time.Second,
	}

	evictor := NewEvictor(manager, cfg, logger)

	// Don't create any hooks

	// Manually trigger memory eviction
	evictor.evictByMemory()

	// Should not panic or error
	if evictor.metrics.EvictionsMemory != 0 {
		t.Errorf("expected 0 memory evictions with no hooks, got %d", evictor.metrics.EvictionsMemory)
	}
}
