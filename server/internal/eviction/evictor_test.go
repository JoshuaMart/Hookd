package eviction

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
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
	manager.CreateHook("example.com", storage.CreateOptions{})
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

	if got := evictor.GetMetrics().EvictionsTTL; got != 1 {
		t.Errorf("expected 1 TTL eviction, got %d", got)
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
	manager.CreateHook("example.com", storage.CreateOptions{})

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

	if got := evictor.GetMetrics().EvictionsLimit; got != 5 {
		t.Errorf("expected 5 limit evictions, got %d", got)
	}
}

func TestEvictor_EvictByHookTTL(t *testing.T) {
	counter := 0
	idGen := func() string { counter++; return fmt.Sprintf("hook-%d", counter) }
	manager := storage.NewMemoryManager(idGen)
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := config.EvictionConfig{
		InteractionTTL:  1 * time.Hour,
		HookTTL:         1 * time.Hour,
		MaxPerHook:      1000,
		MaxMemoryMB:     1800,
		CleanupInterval: 10 * time.Second,
	}
	evictor := NewEvictor(manager, cfg, logger)

	// One hook already past its expiry, one still live, one with no expiry set.
	expired := manager.CreateHook("example.com", storage.CreateOptions{TTL: time.Nanosecond})
	live := manager.CreateHook("example.com", storage.CreateOptions{TTL: time.Hour})
	noExpiry := manager.CreateHook("example.com", storage.CreateOptions{})
	time.Sleep(time.Millisecond) // let the nanosecond TTL lapse

	evictor.evictByHookTTL()

	if _, ok := manager.GetHook(expired.ID); ok {
		t.Error("expected expired hook to be evicted")
	}
	if _, ok := manager.GetHook(live.ID); !ok {
		t.Error("expected live hook to survive")
	}
	if _, ok := manager.GetHook(noExpiry.ID); !ok {
		t.Error("expected hook without expiry to survive")
	}
	if got := evictor.GetMetrics().EvictionsHookTTL; got != 1 {
		t.Errorf("expected 1 hook-TTL eviction, got %d", got)
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
	manager.CreateHook("example.com", storage.CreateOptions{})
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
	hook1 := manager.CreateHook("example1.com", storage.CreateOptions{})
	hook2 := manager.CreateHook("example2.com", storage.CreateOptions{})
	hook3 := manager.CreateHook("example3.com", storage.CreateOptions{})

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
	if evictor.GetMetrics().EvictionsMemory == 0 {
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
	hook := manager.CreateHook("example.com", storage.CreateOptions{})
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

	if got := evictor.GetMetrics().EvictionsMemory; got != 0 {
		t.Errorf("expected 0 memory evictions, got %d", got)
	}
}

// TestEvictor_MetricsConcurrentAccess exercises eviction (writes to the
// counters) and GetMetrics (reads) concurrently. It exists to catch the data
// race that occurred when the counters were plain int64s; run with -race.
func TestEvictor_MetricsConcurrentAccess(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := config.EvictionConfig{
		InteractionTTL:  1 * time.Nanosecond, // everything is immediately expired
		HookTTL:         1 * time.Hour,
		MaxPerHook:      1,
		MaxMemoryMB:     1800,
		CleanupInterval: 10 * time.Second,
	}

	evictor := NewEvictor(manager, cfg, logger)

	hook := manager.CreateHook("example.com", storage.CreateOptions{})

	var wg sync.WaitGroup

	// Writer: repeatedly add interactions and evict them.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			manager.AddInteraction(hook.ID, storage.DNSInteraction("id", "1.2.3.4", "q", "A"))
			evictor.evictByTTL()
		}
	}()

	// Reader: repeatedly snapshot the metrics.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_ = evictor.GetMetrics()
		}
	}()

	wg.Wait()

	if got := evictor.GetMetrics().EvictionsTTL; got == 0 {
		t.Error("expected some TTL evictions to have been recorded")
	}
}

// TestEvictor_EvictByMemory_NoGCBelowThreshold proves that when heap usage is
// below the threshold, evictByMemory does not force a GC and evicts nothing.
func TestEvictor_EvictByMemory_NoGCBelowThreshold(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := config.EvictionConfig{
		InteractionTTL:  1 * time.Hour,
		MaxPerHook:      1000,
		MaxMemoryMB:     100, // threshold = 90 MB
		CleanupInterval: 10 * time.Second,
	}

	evictor := NewEvictor(manager, cfg, logger)

	gcCalls := 0
	evictor.forceGC = func() { gcCalls++ }
	evictor.heapInUseMB = func() int { return 10 } // well below the 90 MB threshold

	hook := manager.CreateHook("example.com", storage.CreateOptions{})
	manager.AddInteraction(hook.ID, storage.DNSInteraction("int1", "1.2.3.4", "test.com", "A"))

	evictor.evictByMemory()

	if gcCalls != 0 {
		t.Errorf("expected no forced GC below threshold, got %d", gcCalls)
	}
	if got := evictor.GetMetrics().EvictionsMemory; got != 0 {
		t.Errorf("expected 0 memory evictions below threshold, got %d", got)
	}
	if stats := manager.Stats(); stats.HooksActive != 1 {
		t.Errorf("expected hook to remain, got %d active", stats.HooksActive)
	}
}

// TestEvictor_EvictByMemory_StopsAtTarget proves the eviction loop stops once a
// fresh, post-GC reading drops below target, instead of evicting every hook.
func TestEvictor_EvictByMemory_StopsAtTarget(t *testing.T) {
	// Unique IDs so each CreateHook is a distinct map entry.
	counter := 0
	idGen := func() string {
		counter++
		return fmt.Sprintf("hook-%d", counter)
	}
	manager := storage.NewMemoryManager(idGen)
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := config.EvictionConfig{
		InteractionTTL:  1 * time.Hour,
		MaxPerHook:      1000,
		MaxMemoryMB:     100, // threshold = 90 MB, target = 80 MB
		CleanupInterval: 10 * time.Second,
	}

	evictor := NewEvictor(manager, cfg, logger)

	// Create 25 distinct hooks, each with one interaction.
	for i := 0; i < 25; i++ {
		h := manager.CreateHook("example.com", storage.CreateOptions{})
		manager.AddInteraction(h.ID, storage.DNSInteraction("int", "1.2.3.4", "test.com", "A"))
	}

	// Model heap usage as proportional to the number of active hooks: 4 MB per
	// hook. 25 hooks -> 100 MB (above threshold). It drops below target (80 MB)
	// once fewer than 20 hooks remain.
	evictor.forceGC = func() {}
	evictor.heapInUseMB = func() int { return manager.Stats().HooksActive * 4 }

	evictor.evictByMemory()

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
	if got := evictor.GetMetrics().EvictionsMemory; got != 0 {
		t.Errorf("expected 0 memory evictions with no hooks, got %d", got)
	}
}
