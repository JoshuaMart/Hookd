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

	// Has reports whether a hook exists, answered from memory so the capture
	// path never hits disk to decide.
	Has(id string) bool

	// AddInteraction adds an interaction to a hook. Interactions for unknown
	// hooks are silently dropped.
	AddInteraction(hookID string, interaction *Interaction)

	// PollInteractions retrieves and deletes interactions for a hook
	PollInteractions(hookID string) []*Interaction

	// PollInteractionsBatch retrieves and deletes interactions for multiple hooks
	PollInteractionsBatch(hookIDs []string) map[string]*PollResult

	// ReadInteractions returns the interactions with a seq above after, without
	// deleting them. It returns ErrHookNotFound for an unknown hook.
	ReadInteractions(hookID string, after int64) (CursorRead, error)

	// AckInteractions deletes the interactions with a seq up to through and
	// returns how many were removed. It returns ErrHookNotFound for an unknown hook.
	AckInteractions(hookID string, through int64) (int, error)

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

// ErrHookNotFound is returned by cursor operations on an unknown hook.
var ErrHookNotFound = errors.New("hook not found")

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

	// CreateLongLivedHooks is the all-or-nothing batch form: either every hook
	// is persisted, in order, or none is.
	CreateLongLivedHooks(domain string, opts []CreateOptions, maxHooks int) ([]*Hook, error)

	// LongLivedHooks returns every long-lived hook, oldest first.
	LongLivedHooks() ([]*Hook, error)

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
	InteractionsSMTP  int
	Memory            MemoryStats
}

// MemoryManager implements in-memory storage
type MemoryManager struct {
	hooks        map[string]*Hook
	interactions map[string][]*Interaction
	// Per-hook cursor state: last assigned seq, highest seq evicted unacked.
	lastSeq        map[string]int64
	droppedThrough map[string]int64
	mu             sync.RWMutex
	idGenerator    func() string
	maxPerHook     int
	limitEvictions int

	// forceGC and heapInUseMB are injectable so the memory-pressure eviction
	// logic can be tested deterministically without depending on the real heap.
	forceGC     func()
	heapInUseMB func() int
}

// NewMemoryManager creates a new in-memory storage manager
func NewMemoryManager(idGenerator func() string) *MemoryManager {
	return &MemoryManager{
		hooks:          make(map[string]*Hook),
		interactions:   make(map[string][]*Interaction),
		lastSeq:        make(map[string]int64),
		droppedThrough: make(map[string]int64),
		idGenerator:    idGenerator,
		forceGC:        runtime.GC,
		heapInUseMB:    readHeapInUseMB,
	}
}

// SetMaxPerHook enables immediate oldest-first eviction for in-memory hooks.
// A non-positive limit disables it. Configure this before accepting captures;
// existing entries are also trimmed by the periodic EnforcePerHookLimit pass.
func (m *MemoryManager) SetMaxPerHook(max int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxPerHook = max
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

	m.lastSeq[hookID]++
	interaction.Seq = m.lastSeq[hookID]
	entries := append(m.interactions[hookID], interaction)
	if m.maxPerHook > 0 && len(entries) > m.maxPerHook {
		drop := len(entries) - m.maxPerHook
		m.noteDropped(hookID, entries[drop-1].Seq)

		clear(entries[:drop])
		entries = entries[drop:]
		m.limitEvictions += drop
	}
	m.interactions[hookID] = entries
}

// ReadInteractions returns the interactions past the cursor without deleting them.
func (m *MemoryManager) ReadInteractions(hookID string, after int64) (CursorRead, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if _, exists := m.hooks[hookID]; !exists {
		return CursorRead{}, ErrHookNotFound
	}
	result := make([]*Interaction, 0)
	for _, interaction := range m.interactions[hookID] {
		if interaction.Seq > after {
			result = append(result, interaction)
		}
	}
	return CursorRead{Interactions: result, DroppedThrough: m.droppedThrough[hookID]}, nil
}

// AckInteractions deletes the interactions up to and including through.
func (m *MemoryManager) AckInteractions(hookID string, through int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.hooks[hookID]; !exists {
		return 0, ErrHookNotFound
	}
	interactions := m.interactions[hookID]
	kept := make([]*Interaction, 0, len(interactions))
	for _, interaction := range interactions {
		if interaction.Seq > through {
			kept = append(kept, interaction)
		} else {

		}
	}
	m.interactions[hookID] = kept
	return len(interactions) - len(kept), nil
}

// noteDropped records that interactions up to seq were evicted unacknowledged.
// Callers must hold m.mu.
func (m *MemoryManager) noteDropped(hookID string, seq int64) {
	if seq > m.droppedThrough[hookID] {
		m.droppedThrough[hookID] = seq
	}
}

// PollInteractions retrieves and deletes interactions for a hook
func (m *MemoryManager) PollInteractions(hookID string) []*Interaction {
	m.mu.Lock()
	defer m.mu.Unlock()

	interactions, exists := m.interactions[hookID]
	if !exists || len(interactions) == 0 {
		return []*Interaction{}
	}

	// Transfer ownership of the slice; future inserts use a new backing array.
	result := interactions

	m.interactions[hookID] = make([]*Interaction, 0)

	return result
}

// PollInteractionsBatch retrieves and deletes interactions for multiple hooks
func (m *MemoryManager) PollInteractionsBatch(hookIDs []string) map[string]*PollResult {
	m.mu.Lock()
	defer m.mu.Unlock()

	results := make(map[string]*PollResult, len(hookIDs))

	for _, hookID := range hookIDs {
		// A duplicate must not overwrite the first drain with an empty result.
		if _, seen := results[hookID]; seen {
			continue
		}
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

		// Transfer ownership instead of allocating another pointer array.
		result := interactions

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
		filtered := interactions[:0]
		for _, interaction := range interactions {
			if interaction.Timestamp.Before(cutoff) {
				total++

				m.noteDropped(hookID, interaction.Seq)
			} else {
				filtered = append(filtered, interaction)
			}
		}
		// The returned read/poll slices never alias this backing array. Clear
		// removed slots so the GC can reclaim their bodies and metadata.
		clear(interactions[len(filtered):])
		if len(filtered) == 0 {
			filtered = nil
		}
		m.interactions[hookID] = filtered
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
			m.deleteHook(id)
			total++
		}
	}
	return total
}

// deleteHook removes a hook and all its state. Callers must hold m.mu.
func (m *MemoryManager) deleteHook(id string) {

	delete(m.hooks, id)
	delete(m.interactions, id)
	delete(m.lastSeq, id)
	delete(m.droppedThrough, id)
}

// EnforcePerHookLimit trims each hook to at most max interactions, dropping the
// oldest first (the slice is ordered by arrival). The return value also includes
// insert-time evictions since the previous pass, for eviction metrics.
func (m *MemoryManager) EnforcePerHookLimit(max int) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Include insert-time evictions exactly once so the evictor's existing
	// limit metric continues to count every dropped interaction.
	total := m.limitEvictions
	m.limitEvictions = 0
	for hookID, interactions := range m.interactions {
		if len(interactions) > max {
			drop := len(interactions) - max

			m.noteDropped(hookID, interactions[drop-1].Seq)
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
		m.deleteHook(hook.ID)
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
			case InteractionTypeSMTP:
				stats.InteractionsSMTP++
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
