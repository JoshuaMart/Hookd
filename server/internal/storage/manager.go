package storage

import (
	"errors"
	"runtime"
	"time"
)

// Manager defines the interface for storage operations
type Manager interface {
	// CreateHook creates a new hook and returns it
	CreateHook(domain string, opts CreateOptions) *Hook

	// Has reports whether a hook exists, answered from memory so the capture
	// path never hits disk to decide.
	Has(id string) bool

	// AddInteraction adds an interaction to a hook. Interactions for unknown
	// hooks are silently dropped.
	AddInteraction(hookID string, interaction *Interaction)

	// The per-hook reads serve only hooks in scope; any other hook reads
	// exactly like an unknown one. Backends also keep unscoped forms
	// (GetHook, PollInteractions...) for tests, outside this interface.

	// GetOwnedHook retrieves a hook by ID.
	GetOwnedHook(scope Scope, id string) (*Hook, bool)

	// PollOwned retrieves and deletes the interactions of several hooks.
	PollOwned(scope Scope, hookIDs []string) map[string]*PollResult

	// ReadOwned returns the interactions with a seq above after, without
	// deleting them. It returns ErrHookNotFound for an unknown hook.
	ReadOwned(scope Scope, hookID string, after int64) (CursorRead, error)

	// AckOwned deletes the interactions with a seq up to through and returns
	// how many were removed. It returns ErrHookNotFound for an unknown hook.
	AckOwned(scope Scope, hookID string, through int64) (int, error)

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

// ErrOwnerHookLimitReached is returned when one owner reached its share of
// long-lived hooks.
var ErrOwnerHookLimitReached = errors.New("long-lived hook limit reached for this owner")

// ownerCounts tallies the hooks each owner adds in a batch; hooks without an
// owner are not capped per owner.
func ownerCounts(opts []CreateOptions) map[string]int {
	counts := map[string]int{}
	for _, o := range opts {
		if o.Owner != "" {
			counts[o.Owner]++
		}
	}
	return counts
}

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

	// LongLivedHooks returns the long-lived hooks in scope, oldest first.
	LongLivedHooks(scope Scope) ([]*Hook, error)

	// LongLivedActivity returns the long-lived hooks in scope that currently
	// have pending interactions, so a client can discover which ones fired.
	LongLivedActivity(scope Scope) []HookActivity

	// LongLivedCount returns the number of long-lived hooks currently stored.
	LongLivedCount() int
}

// Store is a complete backend as the server wires it.
type Store interface {
	Manager
	LongLivedManager
	Close() error
}

// EvictionLeaser is implemented by backends shared between instances, so a
// single instance runs each eviction pass instead of all of them.
type EvictionLeaser interface {
	// HoldEvictionLease takes or renews the lease for ttl and reports whether
	// this instance holds it.
	HoldEvictionLease(ttl time.Duration) bool
}

// FirstExisting returns the index of the first id naming a hook, or -1.
// Backends that pay a round-trip per lookup check every id in one.
func FirstExisting(m Manager, ids ...string) int {
	if batch, ok := m.(interface{ FirstExisting(ids ...string) int }); ok {
		return batch.FirstExisting(ids...)
	}
	for i, id := range ids {
		if m.Has(id) {
			return i
		}
	}
	return -1
}

// BatchCreator is implemented by backends whose hook creation can fail. It
// creates ephemeral and long-lived hooks in one all-or-nothing step, so
// registration reports the failure instead of returning hooks that do not
// exist. maxLongLived caps the long-lived ones (0 = no cap).
type BatchCreator interface {
	CreateHookBatch(domain string, opts []CreateOptions, maxLongLived int) ([]*Hook, error)
}

// AsyncRecorder is implemented by backends whose writes cost a network
// round-trip. A caller that needs neither the seq nor to know the write
// happened, like the DNS server whose answer does not depend on it, records
// through it instead of waiting.
type AsyncRecorder interface {
	// RecordAsync stores the interaction later, or drops it under overload.
	// The caller must not use the interaction afterwards.
	RecordAsync(hookID string, interaction *Interaction)
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

// runtimeMemoryStats samples this process's memory from the Go runtime.
func runtimeMemoryStats() MemoryStats {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	return MemoryStats{
		AllocMB:     int(memStats.Alloc / (1024 * 1024)),
		HeapInuseMB: int(memStats.HeapInuse / (1024 * 1024)),
		SysMB:       int(memStats.Sys / (1024 * 1024)),
		GCRuns:      memStats.NumGC,
	}
}
