package storage

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestCleanupCompactionPreservesCursorSnapshot(t *testing.T) {
	m := NewMemoryManager(func() string { return "hook" })
	id := m.CreateHook("example.test", CreateOptions{}).ID
	// Timestamps need not be ordered by insertion/seq.
	for i, stamp := range []int64{100, 1, 100, 1} {
		m.AddInteraction(id, &Interaction{ID: string(rune('a' + i)), Timestamp: time.Unix(stamp, 0)})
	}
	snapshot, _ := m.ReadInteractions(id, 0)
	backing := m.interactions[id]
	if got := m.EvictInteractionsBefore(time.Unix(50, 0)); got != 2 {
		t.Fatal(got)
	}
	read, _ := m.ReadInteractions(id, 0)
	if len(read.Interactions) != 2 || read.Interactions[0].Seq != 1 || read.Interactions[1].Seq != 3 || read.DroppedThrough != 4 {
		t.Fatalf("bad read: %+v", read)
	}
	if len(snapshot.Interactions) != 4 || snapshot.Interactions[1].Seq != 2 {
		t.Fatal("snapshot changed")
	}
	for _, it := range backing[2:] {
		if it != nil {
			t.Fatal("removed reference retained")
		}
	}
	m.EvictInteractionsBefore(time.Unix(200, 0))
	if m.interactions[id] != nil {
		t.Fatal("empty backing array retained")
	}
}

func TestInsertLimitPreservesNewestAndReportsDropsOnce(t *testing.T) {
	m := NewMemoryManager(func() string { return "hook" })
	m.SetMaxPerHook(3)
	id := m.CreateHook("example.test", CreateOptions{}).ID
	for i := 0; i < 10; i++ {
		m.AddInteraction(id, &Interaction{Type: InteractionTypeDNS})
		if len(m.interactions[id]) > 3 {
			t.Fatal("limit exceeded")
		}
	}
	read, _ := m.ReadInteractions(id, 0)
	if len(read.Interactions) != 3 || read.Interactions[0].Seq != 8 || read.Interactions[2].Seq != 10 || read.DroppedThrough != 7 {
		t.Fatalf("bad read: %+v", read)
	}
	if n := m.EnforcePerHookLimit(3); n != 7 {
		t.Fatal(n)
	}
	if n := m.EnforcePerHookLimit(3); n != 0 {
		t.Fatal(n)
	}
	if n, err := m.AckInteractions(id, 9); err != nil || n != 2 {
		t.Fatalf("ack: %d, %v", n, err)
	}
	m.AddInteraction(id, &Interaction{})
	if got := m.PollInteractions(id); len(got) != 2 || got[1].Seq != 11 {
		t.Fatal(got)
	}
	m.AddInteraction(id, &Interaction{})
	if got := m.PollInteractions(id); len(got) != 1 || got[0].Seq != 12 {
		t.Fatal(got)
	}
}

func TestSQLiteReadersDoNotOccupyWriterAndKeepSnapshot(t *testing.T) {
	m := newTestSQLite(t, 1024)
	id := m.CreateHook("example.test", CreateOptions{}).ID
	m.AddInteraction(id, &Interaction{ID: "first", Type: InteractionTypeDNS})
	tx, err := m.readDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var before int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM interactions`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	done := make(chan int64, 1)
	go func() {
		it := &Interaction{ID: "second", Type: InteractionTypeDNS}
		m.AddInteraction(id, it)
		done <- it.Seq
	}()
	select {
	case seq := <-done:
		if seq != 2 {
			t.Fatal(seq)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader blocked writer")
	}
	var during int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM interactions`).Scan(&during); err != nil {
		t.Fatal(err)
	}
	if before != 1 || during != 1 {
		t.Fatalf("snapshot changed: %d -> %d", before, during)
	}
	if _, err := tx.Exec(`DELETE FROM interactions`); err == nil {
		t.Fatal("read pool allowed a write")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	read, err := m.ReadInteractions(id, 0)
	if err != nil || len(read.Interactions) != 2 {
		t.Fatalf("fresh read: %+v, %v", read, err)
	}
}

func TestBatchDuplicateDoesNotDiscardDrainedInteractions(t *testing.T) {
	disk := newTestSQLite(t, 1024)
	memory := NewMemoryManager(perfIDs())
	composite := NewCompositeManager(memory, disk, time.Hour)
	for _, store := range []Manager{memory, disk, composite} {
		hook := store.CreateHook("example.test", CreateOptions{TTL: 2 * time.Hour})
		store.AddInteraction(hook.ID, &Interaction{ID: "entry-" + hook.ID, Type: InteractionTypeDNS})
		result := store.PollInteractionsBatch([]string{hook.ID, "missing", hook.ID})
		if len(result) != 2 || len(result[hook.ID].Interactions) != 1 || result["missing"].Error == "" {
			t.Fatalf("bad result: %+v", result)
		}
		if got := store.PollInteractions(hook.ID); len(got) != 0 {
			t.Fatal("batch did not drain")
		}
	}
}

func TestSQLiteBatchFailureRollsBackWholeChunk(t *testing.T) {
	m := newTestSQLite(t, 1024)
	first := m.CreateHook("example.test", CreateOptions{}).ID
	second := m.CreateHook("example.test", CreateOptions{}).ID
	m.AddInteraction(first, &Interaction{ID: "good", Type: InteractionTypeDNS})
	m.AddInteraction(second, &Interaction{ID: "bad", Type: InteractionTypeDNS})
	if _, err := m.db.Exec(`UPDATE interactions SET data = '{' WHERE id = 'bad'`); err != nil {
		t.Fatal(err)
	}
	results := m.PollInteractionsBatch([]string{first, second})
	if results[first].Error == "" || results[second].Error == "" {
		t.Fatal("batch error hidden")
	}
	if got := m.PollInteractions(first); len(got) != 1 {
		t.Fatal("failed chunk lost earlier hook")
	}
	var n int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM interactions WHERE id = 'bad'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("corrupt entry lost: %d %v", n, err)
	}
}

func TestPendingCountsTrackAllRemovals(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store Manager
			if backend == "memory" {
				m := NewMemoryManager(perfIDs())
				m.SetMaxPerHook(3)
				store = m
			} else {
				store = newTestSQLite(t, 1024)
			}
			first := store.CreateHook("example.test", CreateOptions{TTL: time.Hour}).ID
			second := store.CreateHook("example.test", CreateOptions{TTL: time.Hour}).ID
			check := func() {
				t.Helper()
				want := Stats{}
				for _, id := range []string{first, second} {
					read, err := store.ReadInteractions(id, -1)
					if err == ErrHookNotFound {
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
					want.HooksActive++
					for _, it := range read.Interactions {
						want.InteractionsTotal++
						switch it.Type {
						case InteractionTypeDNS:
							want.InteractionsDNS++
						case InteractionTypeHTTP:
							want.InteractionsHTTP++
						case InteractionTypeSMTP:
							want.InteractionsSMTP++
						}
					}
				}
				got := store.Stats()
				got.Memory = MemoryStats{}
				if got != want {
					t.Fatalf("counts: got %+v, want %+v", got, want)
				}
			}
			types := []InteractionType{InteractionTypeDNS, InteractionTypeHTTP, InteractionTypeSMTP, "other"}
			for i := 0; i < 20; i++ {
				id := first
				if i%2 == 0 {
					id = second
				}
				store.AddInteraction(id, &Interaction{ID: fmt.Sprint(i), Type: types[i%4], Timestamp: time.Unix(int64(i), 0)})
				check()
			}
			store.AckInteractions(first, 9)
			check()
			store.EnforcePerHookLimit(2)
			check()
			store.EvictInteractionsBefore(time.Unix(18, 0))
			check()
			store.PollInteractions(first)
			check()
			store.PollInteractionsBatch([]string{second, second})
			check()
			store.AddInteraction(first, &Interaction{ID: "expire", Type: InteractionTypeSMTP})
			check()
			store.EvictExpiredHooks(time.Now().Add(2 * time.Hour))
			check()
		})
	}
}

func TestMemoryDrainedSliceSurvivesLaterMutations(t *testing.T) {
	m := NewMemoryManager(perfIDs())
	m.SetMaxPerHook(2)
	id := m.CreateHook("example.test", CreateOptions{}).ID
	for i := 0; i < 2; i++ {
		m.AddInteraction(id, &Interaction{ID: fmt.Sprint(i)})
	}
	drained := m.PollInteractions(id)
	for i := 2; i < 20; i++ {
		m.AddInteraction(id, &Interaction{ID: fmt.Sprint(i)})
	}
	m.EvictInteractionsBefore(time.Now())
	if len(drained) != 2 || drained[0].ID != "0" || drained[1].ID != "1" {
		t.Fatal("drained slice changed")
	}
	if empty := m.PollInteractions(id); empty == nil || len(empty) != 0 {
		t.Fatal("empty poll must encode as []")
	}
}

func TestConcurrentCaptureReadAndAck(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store Manager
			if backend == "memory" {
				store = NewMemoryManager(perfIDs())
			} else {
				store = newTestSQLite(t, 1024)
			}
			id := store.CreateHook("example.test", CreateOptions{}).ID
			var wg sync.WaitGroup
			for writer := 0; writer < 4; writer++ {
				wg.Add(1)
				go func(writer int) {
					defer wg.Done()
					for i := 0; i < 100; i++ {
						it := &Interaction{ID: fmt.Sprintf("%d-%d", writer, i), Type: InteractionTypeDNS}
						store.AddInteraction(id, it)
						if it.Seq == 0 {
							t.Error("capture lost")
						}
					}
				}(writer)
			}
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			var cursor int64
			var seen int
			deadline := time.Now().Add(10 * time.Second)
			for {
				if time.Now().After(deadline) {
					t.Fatal("concurrent drain timed out")
				}
				read, err := store.ReadInteractions(id, cursor)
				if err != nil {
					t.Fatal(err)
				}
				for _, it := range read.Interactions {
					if it.Seq != cursor+1 {
						t.Fatalf("sequence gap: %d -> %d", cursor, it.Seq)
					}
					cursor = it.Seq
					seen++
				}
				if _, err := store.AckInteractions(id, cursor); err != nil {
					t.Fatal(err)
				}
				store.Stats()
				select {
				case <-done:
					if cursor == 400 {
						if seen != 400 || store.Stats().InteractionsTotal != 0 {
							t.Fatal("incorrect drain counts")
						}
						return
					}
				default:
				}
				if cursor > 400 {
					t.Fatal("extra interaction")
				}
			}
		})
	}
}
