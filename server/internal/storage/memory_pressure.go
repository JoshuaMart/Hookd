package storage

import (
	"runtime"
	"sort"
)

// Memory-pressure thresholds, as fractions of eviction.max_memory_mb: evict
// above the first, down to the second.
const (
	pressureThreshold = 0.9
	pressureTarget    = 0.8
	// HeapInuse only drops after a GC, so re-measuring makes sense only in
	// batches: each batch boundary forces one GC and checks a fresh reading.
	// This bounds both the number of forced GCs and how far we can over-evict.
	pressureBatch = 10
	// minShedBytes is the smallest round of data shedding: below it, a round
	// sheds everything left rather than forcing yet another GC for crumbs.
	minShedBytes = 1 << 20
	// maxShedRounds bounds one pass of data shedding.
	maxShedRounds = 32
)

// EvictByMemoryPressure is the safety net behind the byte budget, for when
// the heap nears the configured limit anyway (the budget is an estimate). It
// sheds captured data from the heaviest hooks first, so the pressure falls on
// whoever holds the memory; registrations are deleted only as a last resort,
// once no captured data is left to shed. It also reports, and resets, the
// interactions the byte budget dropped at insert time since the last call.
func (m *MemoryManager) EvictByMemoryPressure(maxMemoryMB int) MemoryEvictionResult {
	m.mu.Lock()
	result := MemoryEvictionResult{BudgetEvicted: m.budgetEvictions}
	m.budgetEvictions = 0
	m.mu.Unlock()

	heap, pressured := m.heapAbove(int(float64(maxMemoryMB) * pressureThreshold))
	if !pressured {
		return result
	}
	// Genuine pressure: report it even if nothing turns out to be evictable, so
	// the evictor can log an early-warning before a possible OOM.
	result.Triggered = true
	result.HeapInUseMB = heap
	target := int(float64(maxMemoryMB) * pressureTarget)
	if m.shedDataUntil(target, &result) || m.storedBytes() > 0 {
		// Below target, or data is still arriving: shedding resumes next
		// tick, and registrations are left alone while data remains.
		return result
	}
	m.evictOldestUntil(target, &result)
	return result
}

// shedDataUntil sheds captured data heaviest-first, half of what is stored per
// round, re-measuring the heap after each, and reports whether the heap got
// below target. Halving bounds the rounds (and forced GCs) logarithmically
// while never shedding much more than needed.
func (m *MemoryManager) shedDataUntil(target int, result *MemoryEvictionResult) bool {
	// Captures keep arriving between rounds; the cap keeps a sustained flood
	// from pinning the evictor here. The next tick resumes.
	for round := 0; round < maxShedRounds; round++ {
		m.mu.Lock()
		stored := m.totalBytes
		if stored <= 0 {
			m.mu.Unlock()
			return false
		}
		chunk := stored / 2
		if chunk < minShedBytes {
			chunk = stored
		}
		result.InteractionsEvicted += m.shedHeaviest(chunk)
		m.mu.Unlock()

		m.forceGC()
		result.HeapInUseMB = m.heapInUseMB()
		if result.HeapInUseMB < target {
			return true
		}
	}
	return false
}

// storedBytes reads the bytes held by captured interactions.
func (m *MemoryManager) storedBytes() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.totalBytes
}

// heapAbove reports the heap and whether it exceeds threshold after a GC.
func (m *MemoryManager) heapAbove(threshold int) (int, bool) {
	// Fast path: read heap usage without forcing a GC. This runs on every
	// cleanup tick, and forcing a stop-the-world GC each time (when there is
	// almost never any pressure) would be wasteful.
	if m.heapInUseMB() < threshold {
		return 0, false
	}
	// A high HeapInuse reading may just be uncollected garbage. Force a GC and
	// re-measure so we only evict on genuine, post-collection pressure.
	m.forceGC()
	heap := m.heapInUseMB()
	return heap, heap >= threshold
}

// evictOldestUntil is the last resort, reached only once no captured data is
// left: it deletes hooks oldest first until the heap is below target,
// recording what it removed and the final heap reading in result.
func (m *MemoryManager) evictOldestUntil(target int, result *MemoryEvictionResult) {
	// result.HeapInUseMB is already a post-GC reading, so the first batch
	// starts from it instead of forcing another collection.
	heap := result.HeapInUseMB
	if heap < target {
		return
	}
	for _, hook := range m.hooksOldestFirst() {
		if result.HooksEvicted > 0 && result.HooksEvicted%pressureBatch == 0 {
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
}

// hooksOldestFirst snapshots the hooks under a brief read lock, released
// before the eviction loop so forceGC/heapInUseMB never run while held.
func (m *MemoryManager) hooksOldestFirst() []*Hook {
	m.mu.RLock()
	hooks := make([]*Hook, 0, len(m.hooks))
	for _, hook := range m.hooks {
		hooks = append(hooks, hook)
	}
	m.mu.RUnlock()
	sort.Slice(hooks, func(i, j int) bool {
		return hooks[i].CreatedAt.Before(hooks[j].CreatedAt)
	})
	return hooks
}

// readHeapInUseMB reports the heap currently in use, in megabytes, without
// forcing a garbage collection.
func readHeapInUseMB() int {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int(m.HeapInuse / (1024 * 1024))
}
