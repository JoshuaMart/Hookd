package storage

import (
	"fmt"
	"strings"
	"testing"
)

const mib = 1 << 20

func budgetIDs() func() string {
	n := 0
	return func() string { n++; return fmt.Sprintf("hook-%d", n) }
}

func bigHTTP(id string, size int) *Interaction {
	return HTTPInteraction(id, "203.0.113.7", "POST", "/", map[string]string{"X": "y"}, strings.Repeat("a", size))
}

// heldBytes sums the estimated bytes of a hook's stored interactions.
func heldBytes(t *testing.T, m *MemoryManager, id string) int64 {
	t.Helper()
	read, err := m.ReadInteractions(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, it := range read.Interactions {
		n += it.memSize
	}
	return n
}

func TestMemoryBudget_PerHookCapTrimsOnlyThatHook(t *testing.T) {
	m := NewMemoryManager(budgetIDs())
	m.SetMemoryBudget(4*mib, 0)
	victim := m.CreateHook("example.test", CreateOptions{}).ID
	attacker := m.CreateHook("example.test", CreateOptions{}).ID
	m.AddInteraction(victim, bigHTTP("v1", 1024))

	for i := 0; i < 50; i++ {
		m.AddInteraction(attacker, bigHTTP(fmt.Sprint("a", i), mib/2))
	}

	if got := heldBytes(t, m, attacker); got > 4*mib {
		t.Errorf("attacker hook holds %d bytes, cap is %d", got, 4*mib)
	}
	read, _ := m.ReadInteractions(attacker, 0)
	if last := read.Interactions[len(read.Interactions)-1]; last.ID != "a49" {
		t.Errorf("expected the newest interaction kept, last is %s", last.ID)
	}
	if read.DroppedThrough == 0 {
		t.Error("expected the dropped interactions to show in the cursor")
	}
	if got := m.PollInteractions(victim); len(got) != 1 {
		t.Errorf("expected the victim's interaction untouched, got %d", len(got))
	}
	if r := m.EvictByMemoryPressure(1 << 20); r.BudgetEvicted == 0 {
		t.Error("expected insert-time budget evictions to be reported")
	} else if again := m.EvictByMemoryPressure(1 << 20); again.BudgetEvicted != 0 {
		t.Errorf("expected budget evictions reported once, got %d again", again.BudgetEvicted)
	}
}

func TestMemoryBudget_TotalShedsHeaviestFirst(t *testing.T) {
	m := NewMemoryManager(budgetIDs())
	m.SetMemoryBudget(4*mib, 10*mib)
	victim := m.CreateHook("example.test", CreateOptions{}).ID
	m.AddInteraction(victim, bigHTTP("v1", 64*1024))
	victimBytes := heldBytes(t, m, victim)

	// Four flooded hooks want 16 MiB together, past the 10 MiB total.
	var flooded []string
	for h := 0; h < 4; h++ {
		id := m.CreateHook("example.test", CreateOptions{}).ID
		flooded = append(flooded, id)
		for i := 0; i < 16; i++ {
			m.AddInteraction(id, bigHTTP(fmt.Sprint(h, "-", i), mib/4))
		}
	}

	if m.Stats().StoredBytes > 10*mib {
		t.Errorf("store holds %d bytes, budget is %d", m.Stats().StoredBytes, 10*mib)
	}
	if got := heldBytes(t, m, victim); got != victimBytes {
		t.Errorf("victim lost data: holds %d, had %d", got, victimBytes)
	}
	for _, id := range flooded {
		if heldBytes(t, m, id) == 0 {
			t.Errorf("hook %s emptied; shedding should level the heavy hooks, not wipe one", id)
		}
	}
}

func TestMemoryBudget_ShedHeaviestLevels(t *testing.T) {
	m := NewMemoryManager(budgetIDs())
	sizes := []int{8, 4, 1} // interactions of 256 KiB each
	var ids []string
	for h, n := range sizes {
		id := m.CreateHook("example.test", CreateOptions{}).ID
		ids = append(ids, id)
		for i := 0; i < n; i++ {
			m.AddInteraction(id, bigHTTP(fmt.Sprint(h, "-", i), mib/4))
		}
	}
	before := []int64{heldBytes(t, m, ids[0]), heldBytes(t, m, ids[1]), heldBytes(t, m, ids[2])}

	m.mu.Lock()
	m.shedHeaviest(before[0] / 2)
	m.mu.Unlock()

	after := []int64{heldBytes(t, m, ids[0]), heldBytes(t, m, ids[1]), heldBytes(t, m, ids[2])}
	if after[2] != before[2] {
		t.Errorf("lightest hook was trimmed: %d -> %d", before[2], after[2])
	}
	if after[0] >= before[0] {
		t.Error("heaviest hook was not trimmed")
	}
	if freed := (before[0] + before[1]) - (after[0] + after[1]); freed < before[0]/2 {
		t.Errorf("freed %d bytes, asked for %d", freed, before[0]/2)
	}
}

func TestMemoryPressure_ShedsDataBeforeHooks(t *testing.T) {
	m := NewMemoryManager(budgetIDs())
	m.forceGC = func() {}
	// Model the heap as the stored bytes plus a little per hook.
	m.heapInUseMB = func() int {
		s := m.Stats()
		return int(s.StoredBytes/mib) + s.HooksActive
	}

	victim := m.CreateHook("example.test", CreateOptions{}).ID // oldest
	m.AddInteraction(victim, bigHTTP("v1", 1024))
	attacker := m.CreateHook("example.test", CreateOptions{}).ID
	for i := 0; i < 100; i++ {
		m.AddInteraction(attacker, bigHTTP(fmt.Sprint("a", i), mib))
	}

	r := m.EvictByMemoryPressure(100) // threshold 90, target 80
	if !r.Triggered {
		t.Fatal("expected memory pressure to trigger")
	}
	if r.HooksEvicted != 0 {
		t.Errorf("expected no hook deleted while data could be shed, got %d", r.HooksEvicted)
	}
	if r.HeapInUseMB >= 80 {
		t.Errorf("expected heap below target, got %d", r.HeapInUseMB)
	}
	if got := m.PollInteractions(victim); len(got) != 1 {
		t.Errorf("expected the victim's data kept, got %d interactions", len(got))
	}
	if !m.Has(attacker) {
		t.Error("expected the flooded hook's registration kept; only its data goes")
	}
}

func TestMemoryPressure_DeletesHooksOnlyWithoutData(t *testing.T) {
	m := NewMemoryManager(budgetIDs())
	m.forceGC = func() {}
	m.heapInUseMB = func() int { return 95 } // pressure no shedding can relieve

	h := m.CreateHook("example.test", CreateOptions{}).ID
	m.AddInteraction(h, bigHTTP("x", mib))

	r := m.EvictByMemoryPressure(100)
	if r.InteractionsEvicted != 1 {
		t.Errorf("expected the data shed first, evicted %d", r.InteractionsEvicted)
	}
	if r.HooksEvicted != 1 {
		t.Errorf("expected the hook deleted as a last resort, got %d", r.HooksEvicted)
	}
}

func TestMemoryPressure_KeepsHooksWhileDataArrives(t *testing.T) {
	m := NewMemoryManager(budgetIDs())
	m.forceGC = func() {}
	h := m.CreateHook("example.test", CreateOptions{}).ID
	n := 0
	// Each heap reading coincides with a new capture: a sustained flood.
	m.heapInUseMB = func() int {
		n++
		m.AddInteraction(h, bigHTTP(fmt.Sprint(n), mib))
		return 95
	}

	if r := m.EvictByMemoryPressure(100); r.HooksEvicted != 0 {
		t.Errorf("expected registrations kept while data remains, %d deleted", r.HooksEvicted)
	}
	if !m.Has(h) {
		t.Error("expected the hook to survive")
	}
}

func TestMemoryBudget_SharedBodyCountedOnce(t *testing.T) {
	m := NewMemoryManager(budgetIDs())
	a := m.CreateHook("example.test", CreateOptions{}).ID
	b := m.CreateHook("example.test", CreateOptions{}).ID
	body := strings.Repeat("m", mib)
	// One message to two recipients: two interactions, one body in memory.
	m.AddInteraction(a, SMTPInteraction("1", "203.0.113.7", "h", "f@x", "a@x", "", "s", body))
	m.AddInteraction(b, SMTPInteraction("2", "203.0.113.7", "h", "f@x", "b@x", "", "s", body))

	stored := m.Stats().StoredBytes
	if stored >= 2*mib {
		t.Errorf("shared body counted twice: %d bytes stored", stored)
	}
	if heldBytes(t, m, a) < mib || heldBytes(t, m, b) < mib {
		t.Error("each hook should still be charged the body it pins")
	}

	m.PollInteractions(a)
	if got := m.Stats().StoredBytes; got < mib {
		t.Errorf("body released while hook b still holds it: %d bytes stored", got)
	}
	m.PollInteractions(b)
	if got := m.Stats().StoredBytes; got != 0 {
		t.Errorf("expected nothing stored, got %d", got)
	}
}

func TestMemoryBudget_KeepsNewestOversizedCapture(t *testing.T) {
	m := NewMemoryManager(budgetIDs())
	m.SetMemoryBudget(mib/2, 0)
	h := m.CreateHook("example.test", CreateOptions{}).ID
	m.AddInteraction(h, bigHTTP("old", mib))
	m.AddInteraction(h, bigHTTP("new", mib))

	got := m.PollInteractions(h)
	if len(got) != 1 || got[0].ID != "new" {
		t.Fatalf("expected only the newest capture kept, got %d", len(got))
	}
}

func TestMemoryBudget_GlobalShedReportsDrops(t *testing.T) {
	m := NewMemoryManager(budgetIDs())
	m.SetMemoryBudget(0, 4*mib)
	heavy := m.CreateHook("example.test", CreateOptions{}).ID
	for i := 0; i < 8; i++ {
		m.AddInteraction(heavy, bigHTTP(fmt.Sprint(i), mib))
	}
	read, err := m.ReadInteractions(heavy, 0)
	if err != nil {
		t.Fatal(err)
	}
	if read.DroppedThrough == 0 {
		t.Error("expected global shedding to show in dropped_through")
	}
}

func TestMemoryPressure_ShedReportsDrops(t *testing.T) {
	m := NewMemoryManager(budgetIDs())
	m.forceGC = func() {}
	m.heapInUseMB = func() int { return int(m.Stats().StoredBytes / mib) }
	h := m.CreateHook("example.test", CreateOptions{})
	for i := 0; i < 100; i++ {
		m.AddInteraction(h.ID, bigHTTP(fmt.Sprint(i), mib))
	}

	m.EvictByMemoryPressure(100)

	read, err := m.ReadInteractions(h.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if read.DroppedThrough == 0 {
		t.Error("expected pressure shedding to show in dropped_through")
	}
}
