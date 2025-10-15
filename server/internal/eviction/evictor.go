package eviction

import (
	"context"
	"log/slog"
	"runtime"
	"time"

	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/storage"
)

// Evictor manages eviction of old interactions
type Evictor struct {
	storage storage.Manager
	config  config.EvictionConfig
	logger  *slog.Logger
	metrics *Metrics
}

// Metrics tracks eviction statistics
type Metrics struct {
	EvictionsTTL     int64
	EvictionsLimit   int64
	EvictionsMemory  int64
	EvictionsHookTTL int64
}

// NewEvictor creates a new evictor
func NewEvictor(storage storage.Manager, cfg config.EvictionConfig, logger *slog.Logger) *Evictor {
	return &Evictor{
		storage: storage,
		config:  cfg,
		logger:  logger,
		metrics: &Metrics{},
	}
}

// Start starts the eviction loop
func (e *Evictor) Start(ctx context.Context) {
	ticker := time.NewTicker(e.config.CleanupInterval)
	defer ticker.Stop()

	e.logger.Info("eviction system started",
		"interval", e.config.CleanupInterval,
		"interaction_ttl", e.config.InteractionTTL,
		"hook_ttl", e.config.HookTTL,
		"max_per_hook", e.config.MaxPerHook,
		"max_memory_mb", e.config.MaxMemoryMB)

	for {
		select {
		case <-ctx.Done():
			e.logger.Info("eviction system stopped")
			return
		case <-ticker.C:
			e.runEviction()
		}
	}
}

// runEviction performs all eviction strategies
func (e *Evictor) runEviction() {
	// 1. TTL-based eviction (interactions)
	e.evictByTTL()

	// 2. Hook TTL-based eviction
	e.evictByHookTTL()

	// 3. Per-hook limit eviction
	e.evictByLimit()

	// 4. Memory pressure eviction
	e.evictByMemory()
}

// evictByTTL removes interactions older than the configured TTL
func (e *Evictor) evictByTTL() {
	now := time.Now().UTC()
	cutoff := now.Add(-e.config.InteractionTTL)

	allInteractions := e.storage.GetAllInteractions()
	totalEvicted := 0

	for hookID, interactions := range allInteractions {
		toDelete := make([]string, 0)

		for _, interaction := range interactions {
			if interaction.Timestamp.Before(cutoff) {
				toDelete = append(toDelete, interaction.ID)
			}
		}

		if len(toDelete) > 0 {
			e.storage.DeleteInteractions(hookID, toDelete)
			totalEvicted += len(toDelete)
		}
	}

	if totalEvicted > 0 {
		e.metrics.EvictionsTTL += int64(totalEvicted)
		e.logger.Debug("ttl eviction completed", "evicted", totalEvicted)
	}
}

// evictByHookTTL removes hooks older than the configured hook TTL
func (e *Evictor) evictByHookTTL() {
	now := time.Now().UTC()
	cutoff := now.Add(-e.config.HookTTL)

	allHooks := e.storage.GetAllHooks()
	totalEvicted := 0

	for _, hook := range allHooks {
		if hook.CreatedAt.Before(cutoff) {
			e.storage.DeleteHook(hook.ID)
			totalEvicted++
		}
	}

	if totalEvicted > 0 {
		e.metrics.EvictionsHookTTL += int64(totalEvicted)
		e.logger.Info("hook ttl eviction completed", "evicted_hooks", totalEvicted)
	}
}

// evictByLimit enforces max interactions per hook (FIFO)
func (e *Evictor) evictByLimit() {
	allInteractions := e.storage.GetAllInteractions()
	totalEvicted := 0

	for hookID, interactions := range allInteractions {
		if len(interactions) > e.config.MaxPerHook {
			// Calculate how many to evict
			toEvict := len(interactions) - e.config.MaxPerHook

			// Collect IDs of oldest interactions (assuming slice is ordered by timestamp)
			toDelete := make([]string, 0, toEvict)
			for i := 0; i < toEvict; i++ {
				toDelete = append(toDelete, interactions[i].ID)
			}

			e.storage.DeleteInteractions(hookID, toDelete)
			totalEvicted += len(toDelete)
		}
	}

	if totalEvicted > 0 {
		e.metrics.EvictionsLimit += int64(totalEvicted)
		e.logger.Debug("limit eviction completed", "evicted", totalEvicted)
	}
}

// evictByMemory performs emergency eviction when approaching memory limit
func (e *Evictor) evictByMemory() {
	// Force GC to get accurate memory reading
	runtime.GC()

	stats := e.storage.Stats()

	// Check if we're approaching the limit (90% threshold)
	threshold := int(float64(e.config.MaxMemoryMB) * 0.9)

	if stats.Memory.HeapInuseMB < threshold {
		return
	}

	e.logger.Warn("memory pressure detected",
		"heap_inuse_mb", stats.Memory.HeapInuseMB,
		"alloc_mb", stats.Memory.AllocMB,
		"sys_mb", stats.Memory.SysMB,
		"threshold_mb", threshold,
		"max_mb", e.config.MaxMemoryMB)

	// Evict oldest hooks until we're below 80% of limit
	target := int(float64(e.config.MaxMemoryMB) * 0.8)
	totalEvicted := 0
	hooksEvicted := 0

	// Get all hooks sorted by creation time (oldest first)
	hooks := e.storage.GetAllHooks()
	if len(hooks) == 0 {
		return
	}

	// Sort hooks by CreatedAt (oldest first)
	sortedHooks := make([]*storage.Hook, len(hooks))
	copy(sortedHooks, hooks)

	// Simple bubble sort (sufficient for small datasets)
	for i := 0; i < len(sortedHooks)-1; i++ {
		for j := 0; j < len(sortedHooks)-i-1; j++ {
			if sortedHooks[j].CreatedAt.After(sortedHooks[j+1].CreatedAt) {
				sortedHooks[j], sortedHooks[j+1] = sortedHooks[j+1], sortedHooks[j]
			}
		}
	}

	// Get all interactions once (optimization to avoid repeated calls)
	allInteractions := e.storage.GetAllInteractions()

	// Delete oldest hooks until memory is below target
	for _, hook := range sortedHooks {
		if stats.Memory.HeapInuseMB < target {
			break
		}

		// Get interaction count for this hook
		interactionCount := len(allInteractions[hook.ID])

		e.storage.DeleteHook(hook.ID)
		totalEvicted += interactionCount
		hooksEvicted++

		// Recalculate stats every 10 hooks for better performance
		if hooksEvicted%10 == 0 {
			runtime.GC()
			stats = e.storage.Stats()
		}
	}

	// Final stats check
	runtime.GC()
	stats = e.storage.Stats()

	if totalEvicted > 0 {
		e.metrics.EvictionsMemory += int64(totalEvicted)
		e.logger.Warn("memory eviction completed",
			"evicted_interactions", totalEvicted,
			"evicted_hooks", hooksEvicted,
			"new_heap_inuse_mb", stats.Memory.HeapInuseMB,
			"new_alloc_mb", stats.Memory.AllocMB,
			"gc_runs", stats.Memory.GCRuns)
	}
}

// GetMetrics returns eviction metrics
func (e *Evictor) GetMetrics() Metrics {
	return *e.metrics
}
