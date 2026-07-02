package storage

import (
	"errors"
	"time"
)

// CompositeManager routes storage operations between an in-memory store for
// ephemeral hooks and a SQLite store for durable long-lived hooks. Ephemeral
// hooks stay entirely in memory (the hot path is unchanged); only long-lived
// hooks touch disk.
//
// Routing:
//   - CreateHook picks the backend from the requested TTL: a TTL greater than
//     the ephemeral hook TTL means long-lived (SQLite), otherwise memory.
//   - Every other per-hook operation routes by ID, using the SQLite store's
//     in-memory membership set so captures never hit disk to decide.
//
// The long-lived store may be nil (feature disabled), in which case every
// operation falls through to memory.
type CompositeManager struct {
	memory             *MemoryManager
	longLived          *SQLiteManager
	ephemeralThreshold time.Duration
}

// Compile-time checks that CompositeManager satisfies the storage interfaces.
var (
	_ Manager          = (*CompositeManager)(nil)
	_ LongLivedManager = (*CompositeManager)(nil)
)

// NewCompositeManager builds a composite over the given stores. longLived may be
// nil to disable durable hooks. ephemeralThreshold is the ephemeral hook TTL:
// registrations asking for a longer TTL are stored as long-lived.
func NewCompositeManager(memory *MemoryManager, longLived *SQLiteManager, ephemeralThreshold time.Duration) *CompositeManager {
	return &CompositeManager{
		memory:             memory,
		longLived:          longLived,
		ephemeralThreshold: ephemeralThreshold,
	}
}

// routeFor returns the store that owns the given hook ID.
func (c *CompositeManager) routeFor(id string) Manager {
	if c.longLived != nil && c.longLived.Has(id) {
		return c.longLived
	}
	return c.memory
}

// has reports whether either store owns the given hook ID, without a database
// round-trip: the long-lived membership set and the in-memory map are both
// O(1) lookups.
func (c *CompositeManager) has(id string) bool {
	if c.longLived != nil && c.longLived.Has(id) {
		return true
	}
	_, ok := c.memory.GetHook(id)
	return ok
}

// CreateHook stores the hook in the long-lived backend when its TTL exceeds the
// ephemeral threshold, otherwise in memory.
func (c *CompositeManager) CreateHook(domain string, opts CreateOptions) *Hook {
	if c.longLived != nil && opts.TTL > c.ephemeralThreshold {
		return c.longLived.CreateHook(domain, opts)
	}
	return c.memory.CreateHook(domain, opts)
}

// GetHook retrieves a hook from whichever store owns it.
func (c *CompositeManager) GetHook(id string) (*Hook, bool) {
	return c.routeFor(id).GetHook(id)
}

// AddInteraction records an interaction against the hook's owning store.
func (c *CompositeManager) AddInteraction(hookID string, interaction *Interaction) {
	c.routeFor(hookID).AddInteraction(hookID, interaction)
}

// PollInteractions drains interactions from the hook's owning store.
func (c *CompositeManager) PollInteractions(hookID string) []*Interaction {
	return c.routeFor(hookID).PollInteractions(hookID)
}

// PollInteractionsBatch polls several hooks, routing each to its owning store.
func (c *CompositeManager) PollInteractionsBatch(hookIDs []string) map[string]*PollResult {
	results := make(map[string]*PollResult, len(hookIDs))
	for _, id := range hookIDs {
		if !c.has(id) {
			results[id] = &PollResult{Error: "Hook not found"}
			continue
		}
		results[id] = &PollResult{Interactions: c.PollInteractions(id)}
	}
	return results
}

// Stats merges counts from both stores. Runtime memory statistics come from the
// in-memory store, which reads them from the Go runtime.
func (c *CompositeManager) Stats() Stats {
	stats := c.memory.Stats()
	if c.longLived != nil {
		ll := c.longLived.Stats()
		stats.HooksActive += ll.HooksActive
		stats.InteractionsTotal += ll.InteractionsTotal
		stats.InteractionsDNS += ll.InteractionsDNS
		stats.InteractionsHTTP += ll.InteractionsHTTP
	}
	return stats
}

// EvictInteractionsBefore applies interaction-TTL eviction. Only ephemeral
// interactions age out this way; long-lived ones are retained until polled or
// their hook expires.
func (c *CompositeManager) EvictInteractionsBefore(cutoff time.Time) int {
	return c.memory.EvictInteractionsBefore(cutoff)
}

// EvictExpiredHooks removes expired hooks from both stores.
func (c *CompositeManager) EvictExpiredHooks(now time.Time) int {
	n := c.memory.EvictExpiredHooks(now)
	if c.longLived != nil {
		n += c.longLived.EvictExpiredHooks(now)
	}
	return n
}

// EnforcePerHookLimit trims per-hook interaction counts in both stores.
func (c *CompositeManager) EnforcePerHookLimit(max int) int {
	n := c.memory.EnforcePerHookLimit(max)
	if c.longLived != nil {
		n += c.longLived.EnforcePerHookLimit(max)
	}
	return n
}

// EvictByMemoryPressure only affects in-heap storage.
func (c *CompositeManager) EvictByMemoryPressure(maxMemoryMB int) MemoryEvictionResult {
	return c.memory.EvictByMemoryPressure(maxMemoryMB)
}

// CreateLongLivedHook persists a durable hook, enforcing the cap atomically. It
// returns an error when the long-lived store is disabled, full, or unwritable.
func (c *CompositeManager) CreateLongLivedHook(domain string, opts CreateOptions, maxHooks int) (*Hook, error) {
	if c.longLived == nil {
		return nil, errors.New("long-lived hooks are disabled")
	}
	return c.longLived.CreateLongLivedHook(domain, opts, maxHooks)
}

// LongLivedActivity lists long-lived hooks with pending interactions.
func (c *CompositeManager) LongLivedActivity() []HookActivity {
	if c.longLived == nil {
		return nil
	}
	return c.longLived.LongLivedActivity()
}

// LongLivedCount returns the number of long-lived hooks stored.
func (c *CompositeManager) LongLivedCount() int {
	if c.longLived == nil {
		return 0
	}
	return c.longLived.LongLivedCount()
}

// Close releases the long-lived store's resources, if any.
func (c *CompositeManager) Close() error {
	if c.longLived != nil {
		return c.longLived.Close()
	}
	return nil
}
