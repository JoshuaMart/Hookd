package storage

import (
	"errors"
	"runtime"
	"sort"
	"sync"
	"time"
)

// Manager defines the interface for storage operations
type Manager interface {
	// CreateHook creates a new hook and returns it
	CreateHook(domain string, opts CreateOptions) *Hook

	// GetHook retrieves a hook by ID
	GetHook(id string) (*Hook, bool)

	// Has reports whether a hook exists, without loading it. Backends answer it
	// from memory so the capture path never hits disk to decide.
	Has(id string) bool

	// AddInteraction adds an interaction to a hook. Interactions for unknown
	// hooks are silently dropped.
	AddInteraction(hookID string, interaction *Interaction)

	// PollInteractions retrieves and deletes interactions for a hook
	PollInteractions(hookID string) []*Interaction

	// PollInteractionsBatch retrieves and deletes interactions for multiple hooks
	PollInteractionsBatch(hookIDs []string) map[string]*PollResult

	// Stats returns storage statistics
	Stats() Stats

	// EvictInteractionsBefore removes interactions with a timestamp older than
	// cutoff and returns how many were removed. Implementations push this down
	// to their own storage rather than exposing every interaction to the caller.
	EvictInteractionsBefore(cutoff time.Time) int

	// EvictExpiredHooks removes hooks whose ExpiresAt has passed (zero ExpiresAt
	// is skipped) and returns how many hooks were removed.
	EvictExpiredHooks(now time.Time) int

	// EnforcePerHookLimit trims each hook to at most max interactions, dropping
	// the oldest first, and returns how many were removed.
	EnforcePerHookLimit(max int) int

	// EvictByMemoryPressure frees memory when heap usage approaches the limit.
	// It only concerns in-heap storage; disk-backed stores are no-ops.
	EvictByMemoryPressure(maxMemoryMB int) MemoryEvictionResult
}

// MemoryEvictionResult reports what a memory-pressure eviction pass did.
// Triggered is set when heap usage crossed the threshold (post-GC), even if
// nothing was evicted, so the evictor can emit an early-warning log before an
// OOM. HeapInUseMB is the heap reading after the pass.
type MemoryEvictionResult struct {
	Triggered           bool
	HeapInUseMB         int
	HooksEvicted        int
	InteractionsEvicted int
}

// ErrHookLimitReached is returned by CreateLongLivedHook when the configured
// long-lived hook cap has been reached.
var ErrHookLimitReached = errors.New("long-lived hook limit reached")

// LongLivedManager is implemented by managers that support durable long-lived
// hooks. It is an optional capability: callers type-assert a Manager to it and
// degrade gracefully (no long-lived support) when the assertion fails.
type LongLivedManager interface {
	// CreateLongLivedHook atomically enforces the maxHooks cap and persists a new
	// long-lived hook. It returns ErrHookLimitReached when the store is full and
	// a non-nil error when persistence fails, so registration can surface the
	// failure instead of handing back a hook that would never capture. A
	// maxHooks <= 0 means "no cap".
	CreateLongLivedHook(domain string, opts CreateOptions, maxHooks int) (*Hook, error)

	// LongLivedActivity returns the long-lived hooks that currently have pending
	// interactions, so a client can discover which ones fired.
	LongLivedActivity() []HookActivity

	// LongLivedCount returns the number of long-lived hooks currently stored.
	LongLivedCount() int
}

// Stats represents storage statistics
type Stats struct {
	HooksActive       int
	InteractionsTotal int
	InteractionsDNS   int
	InteractionsHTTP  int
	Memory            MemoryStats
}

// MemoryManager implements in-memory storage
type MemoryManager struct {
	hooks        map[string]*Hook
	interactions map[string][]*Interaction
	mu           sync.RWMutex
	idGenerator  func() string

	// forceGC and heapInUseMB are injectable so the memory-pressure eviction
	// logic can be tested deterministically without depending on the real heap.
	forceGC     func()
	heapInUseMB func() int
}

// NewMemoryManager creates a new in-memory storage manager
func NewMemoryManager(idGenerator func() string) *MemoryManager {
	return &MemoryManager{
		hooks:        make(map[string]*Hook),
		interactions: make(map[string][]*Interaction),
		idGenerator:  idGenerator,
		forceGC:      runtime.GC,
		heapInUseMB:  readHeapInUseMB,
	}
}

// readHeapInUseMB reports the heap currently in use, in megabytes, without
// forcing a garbage collection.
func readHeapInUseMB() int {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int(m.HeapInuse / (1024 * 1024))
}

// CreateHook creates a new hook
func (m *MemoryManager) CreateHook(domain string, opts CreateOptions) *Hook {
	m.mu.Lock()
	defer m.mu.Unlock()

	hook := newHook(m.idGenerator(), domain, opts)
	m.hooks[hook.ID] = hook
	m.interactions[hook.ID] = make([]*Interaction, 0)

	return hook
}

// GetHook retrieves a hook by ID
func (m *MemoryManager) GetHook(id string) (*Hook, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	hook, exists := m.hooks[id]
	return hook, exists
}

// Has reports whether a hook exists.
func (m *MemoryManager) Has(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	_, exists := m.hooks[id]
	return exists
}

// AddInteraction adds an interaction to a hook. Interactions for non-existent
// hooks are silently ignored (e.g. bots probing random subdomains).
func (m *MemoryManager) AddInteraction(hookID string, interaction *Interaction) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if hook exists
	if _, exists := m.hooks[hookID]; !exists {
		return
	}

	m.interactions[hookID] = append(m.interactions[hookID], interaction)
}

// PollInteractions retrieves and deletes interactions for a hook
func (m *MemoryManager) PollInteractions(hookID string) []*Interaction {
	m.mu.Lock()
	defer m.mu.Unlock()

	interactions, exists := m.interactions[hookID]
	if !exists {
		return []*Interaction{}
	}

	// Return a copy and clear the slice
	result := make([]*Interaction, len(interactions))
	copy(result, interactions)

	m.interactions[hookID] = make([]*Interaction, 0)

	return result
}

// PollInteractionsBatch retrieves and deletes interactions for multiple hooks
func (m *MemoryManager) PollInteractionsBatch(hookIDs []string) map[string]*PollResult {
	m.mu.Lock()
	defer m.mu.Unlock()

	results := make(map[string]*PollResult, len(hookIDs))

	for _, hookID := range hookIDs {
		// Check if hook exists
		if _, exists := m.hooks[hookID]; !exists {
			results[hookID] = &PollResult{
				Error: "Hook not found",
			}
			continue
		}

		// Get interactions
		interactions, exists := m.interactions[hookID]
		if !exists || len(interactions) == 0 {
			results[hookID] = &PollResult{
				Interactions: make([]*Interaction, 0),
			}
			if exists {
				m.interactions[hookID] = make([]*Interaction, 0)
			}
			continue
		}

		// Return a copy and clear the slice
		result := make([]*Interaction, len(interactions))
		copy(result, interactions)

		m.interactions[hookID] = make([]*Interaction, 0)

		results[hookID] = &PollResult{
			Interactions: result,
		}
	}

	return results
}

// EvictInteractionsBefore removes interactions older than cutoff across all
// hooks and returns the number removed.
func (m *MemoryManager) EvictInteractionsBefore(cutoff time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	total := 0
	for hookID, interactions := range m.interactions {
		filtered := make([]*Interaction, 0, len(interactions))
		for _, interaction := range interactions {
			if interaction.Timestamp.Before(cutoff) {
				total++
			} else {
				filtered = append(filtered, interaction)
			}
		}
		if len(filtered) != len(interactions) {
			m.interactions[hookID] = filtered
		}
	}
	return total
}

// EvictExpiredHooks removes hooks whose ExpiresAt has passed. A zero ExpiresAt
// means "no explicit expiry" and is skipped.
func (m *MemoryManager) EvictExpiredHooks(now time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	total := 0
	for id, hook := range m.hooks {
		if !hook.ExpiresAt.IsZero() && hook.ExpiresAt.Before(now) {
			delete(m.hooks, id)
			delete(m.interactions, id)
			total++
		}
	}
	return total
}

// EnforcePerHookLimit trims each hook to at most max interactions, dropping the
// oldest first (the slice is ordered by arrival), and returns the number removed.
func (m *MemoryManager) EnforcePerHookLimit(max int) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	total := 0
	for hookID, interactions := range m.interactions {
		if len(interactions) > max {
			drop := len(interactions) - max
			// Keep the newest max, copying into a fresh slice so the dropped
			// entries can be garbage-collected.
			m.interactions[hookID] = append([]*Interaction(nil), interactions[drop:]...)
			total += drop
		}
	}
	return total
}

// EvictByMemoryPressure performs emergency eviction of the oldest hooks when
// heap usage approaches the configured limit.
func (m *MemoryManager) EvictByMemoryPressure(maxMemoryMB int) MemoryEvictionResult {
	threshold := int(float64(maxMemoryMB) * 0.9)

	// Fast path: read heap usage without forcing a GC. This runs on every
	// cleanup tick, and forcing a stop-the-world GC each time (when there is
	// almost never any pressure) would be wasteful.
	if m.heapInUseMB() < threshold {
		return MemoryEvictionResult{}
	}

	// A high HeapInuse reading may just be uncollected garbage. Force a GC and
	// re-measure so we only evict on genuine, post-collection pressure.
	m.forceGC()
	heap := m.heapInUseMB()
	if heap < threshold {
		return MemoryEvictionResult{}
	}

	// Genuine pressure: report it even if nothing turns out to be evictable, so
	// the evictor can log an early-warning before a possible OOM.
	result := MemoryEvictionResult{Triggered: true, HeapInUseMB: heap}

	// Snapshot hooks oldest-first under a brief read lock. The lock is released
	// before the eviction loop so forceGC/heapInUseMB never run while held.
	m.mu.RLock()
	hooks := make([]*Hook, 0, len(m.hooks))
	for _, hook := range m.hooks {
		hooks = append(hooks, hook)
	}
	m.mu.RUnlock()

	if len(hooks) == 0 {
		return result
	}
	sort.Slice(hooks, func(i, j int) bool {
		return hooks[i].CreatedAt.Before(hooks[j].CreatedAt)
	})

	target := int(float64(maxMemoryMB) * 0.8)

	// HeapInuse only drops after a GC, so re-measuring makes sense only in
	// batches: each batch boundary forces one GC and checks a fresh reading.
	// This bounds both the number of forced GCs and how far we can over-evict.
	const batchSize = 10

	for _, hook := range hooks {
		if result.HooksEvicted%batchSize == 0 {
			m.forceGC()
			if heap = m.heapInUseMB(); heap < target {
				break
			}
		}

		m.mu.Lock()
		result.InteractionsEvicted += len(m.interactions[hook.ID])
		delete(m.hooks, hook.ID)
		delete(m.interactions, hook.ID)
		m.mu.Unlock()
		result.HooksEvicted++
	}

	if result.HooksEvicted > 0 {
		m.forceGC()
		heap = m.heapInUseMB()
	}
	result.HeapInUseMB = heap
	return result
}

// Stats returns storage statistics
func (m *MemoryManager) Stats() Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := Stats{
		HooksActive: len(m.hooks),
	}

	for _, interactions := range m.interactions {
		stats.InteractionsTotal += len(interactions)
		for _, interaction := range interactions {
			switch interaction.Type {
			case InteractionTypeDNS:
				stats.InteractionsDNS++
			case InteractionTypeHTTP:
				stats.InteractionsHTTP++
			}
		}
	}

	// Get detailed memory usage from Go runtime
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	// Populate detailed memory stats
	stats.Memory = MemoryStats{
		AllocMB:     int(memStats.Alloc / (1024 * 1024)),
		HeapInuseMB: int(memStats.HeapInuse / (1024 * 1024)),
		SysMB:       int(memStats.Sys / (1024 * 1024)),
		GCRuns:      memStats.NumGC,
	}

	return stats
}
