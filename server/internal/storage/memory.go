package storage

import (
	"runtime"
	"sync"
	"time"
)

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
	counts         Stats

	// Byte accounting (see memory_budget.go): what each hook holds, what the
	// store holds, their bounds, and insert-time evictions not yet reported.
	hookBytes       map[string]int64
	bodyRefs        map[bodyRef]int
	totalBytes      int64
	maxHookBytes    int64
	maxTotalBytes   int64
	budgetEvictions int

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
		hookBytes:      make(map[string]int64),
		bodyRefs:       make(map[bodyRef]int),
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
	return m.GetOwnedHook(AllOwners(), id)
}

// GetOwnedHook retrieves a hook by ID if it is in scope.
func (m *MemoryManager) GetOwnedHook(scope Scope, id string) (*Hook, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.owned(scope, id)
}

// owned looks a hook up for owner. Callers must hold m.mu.
func (m *MemoryManager) owned(scope Scope, id string) (*Hook, bool) {
	hook, exists := m.hooks[id]
	if !exists || !scope.allows(hook.Owner) {
		return nil, false
	}
	return hook, true
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
	// Sizing walks the headers; it needs no lock, so captures do not wait on it.
	size, body := interactionSize(interaction)
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if hook exists
	if _, exists := m.hooks[hookID]; !exists {
		return
	}

	m.retain(hookID, interaction, size, body)
	m.lastSeq[hookID]++
	interaction.Seq = m.lastSeq[hookID]
	m.interactions[hookID] = append(m.interactions[hookID], interaction)
	if m.maxPerHook > 0 {
		m.limitEvictions += m.trim(hookID, m.maxPerHook)
	}
	m.enforceBudget(hookID)
}

// trim drops a hook's oldest interactions beyond max, records the loss for
// cursor readers, and returns how many went. Callers must hold m.mu.
func (m *MemoryManager) trim(hookID string, max int) int {
	return m.dropOldest(hookID, len(m.interactions[hookID])-max)
}

// ReadInteractions returns the interactions past the cursor without deleting them.
func (m *MemoryManager) ReadInteractions(hookID string, after int64) (CursorRead, error) {
	return m.ReadOwned(AllOwners(), hookID, after)
}

// ReadOwned is ReadInteractions restricted to the hooks in scope.
func (m *MemoryManager) ReadOwned(scope Scope, hookID string, after int64) (CursorRead, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if _, exists := m.owned(scope, hookID); !exists {
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
	return m.AckOwned(AllOwners(), hookID, through)
}

// AckOwned is AckInteractions restricted to the hooks in scope.
func (m *MemoryManager) AckOwned(scope Scope, hookID string, through int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.owned(scope, hookID); !exists {
		return 0, ErrHookNotFound
	}
	// Entries are in seq order, so the acknowledged ones are a prefix.
	entries := m.interactions[hookID]
	n := 0
	for n < len(entries) && entries[n].Seq <= through {
		n++
	}
	if n == 0 {
		return 0, nil
	}
	m.release(hookID, entries[:n])
	// As in dropOldest: read results never alias this array.
	clear(entries[:n])
	m.interactions[hookID] = entries[n:]
	return n, nil
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
	return m.drain(hookID)
}

// PollInteractionsBatch retrieves and deletes interactions for multiple hooks
func (m *MemoryManager) PollInteractionsBatch(hookIDs []string) map[string]*PollResult {
	return m.PollOwned(AllOwners(), hookIDs)
}

// PollOwned is PollInteractionsBatch restricted to the hooks in scope.
func (m *MemoryManager) PollOwned(scope Scope, hookIDs []string) map[string]*PollResult {
	m.mu.Lock()
	defer m.mu.Unlock()

	results := make(map[string]*PollResult, len(hookIDs))
	for _, hookID := range hookIDs {
		// A duplicate must not overwrite the first drain with an empty result.
		if _, seen := results[hookID]; seen {
			continue
		}
		if _, exists := m.owned(scope, hookID); !exists {
			results[hookID] = &PollResult{Error: "Hook not found"}
			continue
		}
		results[hookID] = &PollResult{Interactions: m.drain(hookID)}
	}
	return results
}

// drain returns and removes a hook's interactions, never nil so it encodes
// as []. Callers must hold m.mu.
func (m *MemoryManager) drain(hookID string) []*Interaction {
	entries := m.interactions[hookID]
	if len(entries) == 0 {
		return []*Interaction{}
	}
	m.release(hookID, entries)
	// Transfer ownership of the slice; future inserts use a new backing array.
	m.interactions[hookID] = make([]*Interaction, 0)
	return entries
}

// EvictInteractionsBefore removes interactions older than cutoff across all
// hooks and returns the number removed.
func (m *MemoryManager) EvictInteractionsBefore(cutoff time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	total := 0
	for hookID, interactions := range m.interactions {
		filtered := interactions[:0]
		var expired []*Interaction
		for _, interaction := range interactions {
			if interaction.Timestamp.Before(cutoff) {
				expired = append(expired, interaction)
				m.noteDropped(hookID, interaction.Seq)
			} else {
				filtered = append(filtered, interaction)
			}
		}
		total += len(expired)
		m.release(hookID, expired)
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
	m.release(id, m.interactions[id])
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
	for hookID := range m.interactions {
		total += m.trim(hookID, max)
	}
	return total
}

// countInteraction updates pending counts while m.mu is held.
func (m *MemoryManager) countInteraction(it *Interaction, delta int) {
	m.counts.InteractionsTotal += delta
	switch it.Type {
	case InteractionTypeDNS:
		m.counts.InteractionsDNS += delta
	case InteractionTypeHTTP:
		m.counts.InteractionsHTTP += delta
	case InteractionTypeSMTP:
		m.counts.InteractionsSMTP += delta
	}
}

// Stats snapshots counts under a short lock. Runtime sampling does not hold up
// captures, and no work scales with the number of pending interactions.
func (m *MemoryManager) Stats() Stats {
	m.mu.RLock()
	stats := m.counts
	stats.HooksActive = len(m.hooks)
	stats.StoredBytes = m.totalBytes
	m.mu.RUnlock()
	stats.Memory = runtimeMemoryStats()
	return stats
}
