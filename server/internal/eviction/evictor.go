package eviction

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/storage"
)

// Evictor drives eviction. It owns policy (which cutoffs and limits apply) and
// delegates the actual data removal to the storage layer, which pushes each
// strategy down to its own backend rather than exposing every record here.
type Evictor struct {
	storage storage.Manager
	config  config.EvictionConfig
	logger  *slog.Logger
	metrics counters
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
		storage: storage,
		config:  cfg,
		logger:  logger,
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

// evictByTTL removes interactions older than the configured interaction TTL.
func (e *Evictor) evictByTTL() {
	cutoff := time.Now().UTC().Add(-e.config.InteractionTTL)
	if n := e.storage.EvictInteractionsBefore(cutoff); n > 0 {
		e.metrics.evictionsTTL.Add(int64(n))
		e.logger.Debug("ttl eviction completed", "evicted", n)
	}
}

// evictByHookTTL removes hooks whose expiry has passed. Each hook carries its
// own ExpiresAt (ephemeral hooks get now+hook_ttl at creation, long-lived hooks
// get their requested TTL), so this pass is agnostic to the hook category.
func (e *Evictor) evictByHookTTL() {
	if n := e.storage.EvictExpiredHooks(time.Now().UTC()); n > 0 {
		e.metrics.evictionsHookTTL.Add(int64(n))
		e.logger.Info("hook ttl eviction completed", "evicted_hooks", n)
	}
}

// evictByLimit enforces max interactions per hook (oldest dropped first).
func (e *Evictor) evictByLimit() {
	if n := e.storage.EnforcePerHookLimit(e.config.MaxPerHook); n > 0 {
		e.metrics.evictionsLimit.Add(int64(n))
		e.logger.Debug("limit eviction completed", "evicted", n)
	}
}

// evictByMemory performs emergency eviction when approaching the memory limit.
// The heap-pressure algorithm lives in the in-memory store; disk-backed stores
// are unaffected since their data is not held on the Go heap.
func (e *Evictor) evictByMemory() {
	r := e.storage.EvictByMemoryPressure(e.config.MaxMemoryMB)
	if r.InteractionsEvicted > 0 || r.HooksEvicted > 0 {
		e.metrics.evictionsMemory.Add(int64(r.InteractionsEvicted))
		e.logger.Warn("memory eviction completed",
			"evicted_interactions", r.InteractionsEvicted,
			"evicted_hooks", r.HooksEvicted)
	}
}

// HookTTL returns the configured default lifetime for ephemeral hooks. The API
// handler uses it to stamp each new hook's ExpiresAt at registration time.
func (e *Evictor) HookTTL() time.Duration {
	return e.config.HookTTL
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
