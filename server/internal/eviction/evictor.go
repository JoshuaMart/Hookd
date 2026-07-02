package eviction

import (
	"context"
	"log/slog"
	"runtime"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/storage"
)

// Evictor manages eviction of old interactions
type Evictor struct {
	storage storage.Manager
	config  config.EvictionConfig
	logger  *slog.Logger
	metrics counters

	// forceGC and heapInUseMB are injectable so the memory-eviction logic can
	// be tested deterministically without depending on the real Go heap.
	forceGC     func()
	heapInUseMB func() int
}

// counters holds the live eviction counters. The eviction goroutine writes
// them while the /metrics handler reads them concurrently, so they must be
// accessed atomically.
type counters struct {
	evictionsTTL     atomic.Int64
	evictionsLimit   atomic.Int64
	evictionsMemory  atomic.Int64
	evictionsHookTTL atomic.Int64
}

// Metrics is an immutable snapshot of the eviction counters.
type Metrics struct {
	EvictionsTTL     int64
	EvictionsLimit   int64
	EvictionsMemory  int64
	EvictionsHookTTL int64
}

// NewEvictor creates a new evictor
func NewEvictor(storage storage.Manager, cfg config.EvictionConfig, logger *slog.Logger) *Evictor {
	return &Evictor{
		storage:     storage,
		config:      cfg,
		logger:      logger,
		forceGC:     runtime.GC,
		heapInUseMB: readHeapInUseMB,
	}
}

// readHeapInUseMB reports the heap currently in use, in megabytes. It reads
// runtime memory stats without forcing a garbage collection.
func readHeapInUseMB() int {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int(m.HeapInuse / (1024 * 1024))
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
		e.metrics.evictionsTTL.Add(int64(totalEvicted))
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
		e.metrics.evictionsHookTTL.Add(int64(totalEvicted))
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
		e.metrics.evictionsLimit.Add(int64(totalEvicted))
		e.logger.Debug("limit eviction completed", "evicted", totalEvicted)
	}
}

// evictByMemory performs emergency eviction when approaching the memory limit.
func (e *Evictor) evictByMemory() {
	threshold := int(float64(e.config.MaxMemoryMB) * 0.9)

	// Fast path: read heap usage without forcing a GC. This runs on every
	// cleanup tick, and forcing a stop-the-world GC each time (when there is
	// almost never any pressure) would be wasteful.
	if e.heapInUseMB() < threshold {
		return
	}

	// A high HeapInuse reading may just be uncollected garbage. Force a GC and
	// re-measure so we only evict on genuine, post-collection pressure.
	e.forceGC()
	if e.heapInUseMB() < threshold {
		return
	}

	hooks := e.storage.GetAllHooks()
	if len(hooks) == 0 {
		return
	}

	// Oldest first.
	sort.Slice(hooks, func(i, j int) bool {
		return hooks[i].CreatedAt.Before(hooks[j].CreatedAt)
	})

	stats := e.storage.Stats()
	e.logger.Warn("memory pressure detected",
		"heap_inuse_mb", stats.Memory.HeapInuseMB,
		"alloc_mb", stats.Memory.AllocMB,
		"sys_mb", stats.Memory.SysMB,
		"threshold_mb", threshold,
		"max_mb", e.config.MaxMemoryMB)

	// Evict oldest hooks until we drop below 80% of the limit.
	target := int(float64(e.config.MaxMemoryMB) * 0.8)
	allInteractions := e.storage.GetAllInteractions()

	totalEvicted := 0
	hooksEvicted := 0

	// HeapInuse only drops after a GC, so re-measuring makes sense only in
	// batches: each batch boundary forces one GC and checks a fresh reading.
	// This bounds both the number of forced GCs and how far we can over-evict.
	const batchSize = 10

	for _, hook := range hooks {
		if hooksEvicted%batchSize == 0 {
			e.forceGC()
			if e.heapInUseMB() < target {
				break
			}
		}

		totalEvicted += len(allInteractions[hook.ID])
		e.storage.DeleteHook(hook.ID)
		hooksEvicted++
	}

	if totalEvicted > 0 {
		e.metrics.evictionsMemory.Add(int64(totalEvicted))
		e.forceGC()
		finalStats := e.storage.Stats()
		e.logger.Warn("memory eviction completed",
			"evicted_interactions", totalEvicted,
			"evicted_hooks", hooksEvicted,
			"new_heap_inuse_mb", finalStats.Memory.HeapInuseMB,
			"new_alloc_mb", finalStats.Memory.AllocMB,
			"gc_runs", finalStats.Memory.GCRuns)
	}
}

// GetMetrics returns a snapshot of the eviction metrics
func (e *Evictor) GetMetrics() Metrics {
	return Metrics{
		EvictionsTTL:     e.metrics.evictionsTTL.Load(),
		EvictionsLimit:   e.metrics.evictionsLimit.Load(),
		EvictionsMemory:  e.metrics.evictionsMemory.Load(),
		EvictionsHookTTL: e.metrics.evictionsHookTTL.Load(),
	}
}
