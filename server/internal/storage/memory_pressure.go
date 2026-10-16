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
)

// EvictByMemoryPressure performs emergency eviction of the oldest hooks when
// heap usage approaches the configured limit.
func (m *MemoryManager) EvictByMemoryPressure(maxMemoryMB int) MemoryEvictionResult {
	heap, pressured := m.heapAbove(int(float64(maxMemoryMB) * pressureThreshold))
	if !pressured {
		return MemoryEvictionResult{}
	}
	// Genuine pressure: report it even if nothing turns out to be evictable, so
	// the evictor can log an early-warning before a possible OOM.
	result := MemoryEvictionResult{Triggered: true, HeapInUseMB: heap}
	m.evictOldestUntil(int(float64(maxMemoryMB)*pressureTarget), &result)
	return result
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

// evictOldestUntil deletes hooks oldest first until the heap is below target,
// recording what it removed and the final heap reading in result.
func (m *MemoryManager) evictOldestUntil(target int, result *MemoryEvictionResult) {
	heap := result.HeapInUseMB
	for _, hook := range m.hooksOldestFirst() {
		if result.HooksEvicted%pressureBatch == 0 {
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
