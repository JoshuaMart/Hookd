package storage

import "sort"

// The in-memory store accounts for the bytes each hook holds so that memory is
// bounded where it is consumed. Capture traffic is anonymous: anyone who sees
// a hook ID (it travels inside payloads) can send it data. Bounding each hook,
// and shedding from the heaviest hooks first when the store as a whole is full,
// keeps one flooded hook from pushing other hooks' data out.

const (
	// interactionOverhead approximates the fixed heap cost of an entry: the
	// struct, its Data map and the slice slot pointing at it.
	interactionOverhead = 512
	// mapEntryOverhead approximates the cost of one map entry beyond its key
	// and value bytes.
	mapEntryOverhead = 48
	// budgetLowWater is the fraction of the store budget shed down to once it
	// is exceeded, so the heaviest-first scan runs once per slice of the
	// budget rather than on every insert.
	budgetLowWater = 0.9
)

// SetMemoryBudget bounds the bytes the store holds: perHook for any one hook,
// total for every hook together. A non-positive value disables that bound.
// Configure it before accepting captures.
func (m *MemoryManager) SetMemoryBudget(perHook, total int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxHookBytes = perHook
	m.maxTotalBytes = total
}

// interactionSize estimates an interaction's heap footprint. It is an
// approximation that errs on the high side for the fields captures carry.
func interactionSize(it *Interaction) int64 {
	size := int64(interactionOverhead + len(it.ID) + len(it.SourceIP) + len(it.Type))
	for key, value := range it.Data {
		size += mapEntryOverhead + int64(len(key)) + valueSize(value)
	}
	return size
}

// valueSize estimates the bytes behind one Data value.
func valueSize(value any) int64 {
	switch v := value.(type) {
	case string:
		return int64(len(v))
	case map[string]string:
		var n int64
		for k, s := range v {
			n += mapEntryOverhead + int64(len(k)+len(s))
		}
		return n
	case []string:
		var n int64
		for _, s := range v {
			n += 16 + int64(len(s))
		}
		return n
	default:
		return 16
	}
}

// retain accounts for an interaction entering a hook. Callers must hold m.mu.
func (m *MemoryManager) retain(hookID string, it *Interaction) {
	it.memSize = interactionSize(it)
	m.hookBytes[hookID] += it.memSize
	m.totalBytes += it.memSize
	m.countInteraction(it, 1)
}

// release accounts for interactions leaving a hook. Callers must hold m.mu.
func (m *MemoryManager) release(hookID string, entries []*Interaction) {
	if len(entries) == 0 {
		return
	}
	var freed int64
	for _, it := range entries {
		freed += it.memSize
		m.countInteraction(it, -1)
	}
	m.totalBytes -= freed
	if left := m.hookBytes[hookID] - freed; left > 0 {
		m.hookBytes[hookID] = left
	} else {
		delete(m.hookBytes, hookID)
	}
}

// dropOldest removes a hook's n oldest interactions, records the loss for
// cursor readers, and returns n. Callers must hold m.mu.
func (m *MemoryManager) dropOldest(hookID string, n int) int {
	entries := m.interactions[hookID]
	if n > len(entries) {
		n = len(entries)
	}
	if n <= 0 {
		return 0
	}
	m.noteDropped(hookID, entries[n-1].Seq)
	m.release(hookID, entries[:n])
	// Read results never alias this array and polls take it over, so the
	// slots can be cleared in place for the GC to reclaim their bodies.
	clear(entries[:n])
	m.interactions[hookID] = entries[n:]
	return n
}

// trimBytes drops a hook's oldest interactions until it holds at most limit
// bytes and returns how many went. Callers must hold m.mu.
func (m *MemoryManager) trimBytes(hookID string, limit int64) int {
	excess := m.hookBytes[hookID] - limit
	if excess <= 0 {
		return 0
	}
	n := 0
	for _, it := range m.interactions[hookID] {
		if excess <= 0 {
			break
		}
		excess -= it.memSize
		n++
	}
	return m.dropOldest(hookID, n)
}

// enforceBudget applies the byte bounds after hookID received data: the hook
// is trimmed to its own share, then, if the store is over its total, the
// heaviest hooks are shed. Callers must hold m.mu.
func (m *MemoryManager) enforceBudget(hookID string) {
	if m.maxHookBytes > 0 {
		m.budgetEvictions += m.trimBytes(hookID, m.maxHookBytes)
	}
	if m.maxTotalBytes > 0 && m.totalBytes > m.maxTotalBytes {
		lowWater := int64(float64(m.maxTotalBytes) * budgetLowWater)
		m.budgetEvictions += m.shedHeaviest(m.totalBytes - lowWater)
	}
}

// shedHeaviest frees at least excess bytes by lowering the heaviest hooks to a
// common level, oldest interactions first, so the hooks holding the most pay
// and lighter hooks keep everything. It returns how many interactions went.
// Callers must hold m.mu.
func (m *MemoryManager) shedHeaviest(excess int64) int {
	if excess <= 0 || len(m.hookBytes) == 0 {
		return 0
	}
	type held struct {
		id    string
		bytes int64
	}
	hooks := make([]held, 0, len(m.hookBytes))
	for id, b := range m.hookBytes {
		hooks = append(hooks, held{id, b})
	}
	sort.Slice(hooks, func(i, j int) bool { return hooks[i].bytes > hooks[j].bytes })

	// Water-filling: find the level such that cutting every hook above it
	// down to it frees excess. Lowering the top k hooks to the next hook's
	// size frees sum(top k) - k*next; the first k for which that suffices
	// holds the level.
	level := int64(0)
	var top int64
	for k := range hooks {
		top += hooks[k].bytes
		next := int64(0)
		if k+1 < len(hooks) {
			next = hooks[k+1].bytes
		}
		if top-int64(k+1)*next >= excess {
			level = (top - excess) / int64(k+1)
			break
		}
	}

	dropped := 0
	for _, h := range hooks {
		if h.bytes <= level {
			break
		}
		dropped += m.trimBytes(h.id, level)
	}
	return dropped
}
