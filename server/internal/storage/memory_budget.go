package storage

import (
	"container/heap"
	"unsafe"
)

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

// bodyRef identifies the bytes behind a captured body. An SMTP message sent
// to several recipients yields one interaction each, all sharing the same
// body string, which the heap holds once.
type bodyRef struct {
	data *byte
	len  int
}

// interactionSize estimates an interaction's heap footprint, body included,
// and identifies its body. It is an approximation that errs on the high side
// for the fields captures carry. It reads only the interaction, so callers
// run it before taking the store's lock.
func interactionSize(it *Interaction) (int64, bodyRef) {
	size := int64(interactionOverhead + len(it.ID) + len(it.SourceIP) + len(it.Type))
	var body bodyRef
	for key, value := range it.Data {
		size += mapEntryOverhead + int64(len(key)) + valueSize(value)
		if s, ok := value.(string); ok && key == "body" && s != "" {
			body = bodyRef{unsafe.StringData(s), len(s)}
		}
	}
	return size, body
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

// retain accounts for an interaction entering a hook, with the size and body
// interactionSize computed. The hook is charged in full, since it pins the
// body; the store total counts a body shared by several interactions once.
// Callers must hold m.mu.
func (m *MemoryManager) retain(hookID string, it *Interaction, size int64, body bodyRef) {
	it.memSize, it.memBody = size, body
	m.hookBytes[hookID] += size
	m.totalBytes += size - int64(body.len)
	if body.len > 0 {
		if m.bodyRefs[body]++; m.bodyRefs[body] == 1 {
			m.totalBytes += int64(body.len)
		}
	}
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
		m.totalBytes -= it.memSize - int64(it.memBody.len)
		if body := it.memBody; body.len > 0 {
			if m.bodyRefs[body]--; m.bodyRefs[body] <= 0 {
				delete(m.bodyRefs, body)
				m.totalBytes -= int64(body.len)
			}
		}
		m.countInteraction(it, -1)
	}
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
// bytes, never touching its keep newest ones, and returns how many went.
// Callers must hold m.mu.
func (m *MemoryManager) trimBytes(hookID string, limit int64, keep int) int {
	excess := m.hookBytes[hookID] - limit
	if excess <= 0 {
		return 0
	}
	entries := m.interactions[hookID]
	n := 0
	for n < len(entries)-keep && excess > 0 {
		excess -= entries[n].memSize
		n++
	}
	return m.dropOldest(hookID, n)
}

// enforceBudget applies the byte bounds after hookID received data: the hook
// is trimmed to its own share, then, if the store is over its total, the
// heaviest hooks are shed. The capture just stored is kept even if it alone
// exceeds the cap (validation sizes the cap for several), so a hook always
// shows its latest callback. Callers must hold m.mu.
func (m *MemoryManager) enforceBudget(hookID string) {
	if m.maxHookBytes > 0 {
		m.budgetEvictions += m.trimBytes(hookID, m.maxHookBytes, 1)
	}
	if m.maxTotalBytes > 0 && m.totalBytes > m.maxTotalBytes {
		lowWater := int64(float64(m.maxTotalBytes) * budgetLowWater)
		m.budgetEvictions += m.shedHeaviest(m.totalBytes - lowWater)
	}
}

// shedHeaviest frees at least excess bytes of hook data by lowering the
// heaviest hooks to a common level, oldest interactions first, so the hooks
// holding the most pay and lighter hooks keep everything. It returns how many
// interactions went. Callers must hold m.mu.
func (m *MemoryManager) shedHeaviest(excess int64) int {
	if excess <= 0 || len(m.hookBytes) == 0 {
		return 0
	}
	// A max-heap yields the heaviest hooks one at a time: building it is
	// linear, and only the hooks above the level are ever popped, instead of
	// sorting every hook while captures wait on the lock.
	hooks := make(heldHeap, 0, len(m.hookBytes))
	for id, b := range m.hookBytes {
		hooks = append(hooks, held{id, b})
	}
	heap.Init(&hooks)

	// Water-filling: lowering the k heaviest hooks to the next one's size
	// frees sum(top k) - k*next; the first k for which that covers excess
	// holds the level, and every hook left in the heap is at or below it.
	var popped []held
	var top, level int64
	for hooks.Len() > 0 {
		h := heap.Pop(&hooks).(held)
		popped = append(popped, h)
		top += h.bytes
		next := int64(0)
		if hooks.Len() > 0 {
			next = hooks[0].bytes
		}
		k := int64(len(popped))
		if top-k*next >= excess {
			level = (top - excess) / k
			break
		}
	}

	dropped := 0
	for _, h := range popped {
		dropped += m.trimBytes(h.id, level, 0)
	}
	return dropped
}

// held is a hook and the bytes it holds.
type held struct {
	id    string
	bytes int64
}

// heldHeap orders hooks heaviest first for container/heap.
type heldHeap []held

func (h heldHeap) Len() int           { return len(h) }
func (h heldHeap) Less(i, j int) bool { return h[i].bytes > h[j].bytes }
func (h heldHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *heldHeap) Push(x any)        { *h = append(*h, x.(held)) }
func (h *heldHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}
