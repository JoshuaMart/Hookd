package storage

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func perfIDs() func() string {
	var n atomic.Int64
	return func() string { return fmt.Sprintf("id-%d", n.Add(1)) }
}
func perfMemory(hooks, entries int) *MemoryManager {
	m := NewMemoryManager(perfIDs())
	for h := 0; h < hooks; h++ {
		id := m.CreateHook("example.test", CreateOptions{}).ID
		for i := 0; i < entries; i++ {
			m.AddInteraction(id, &Interaction{Type: InteractionTypeDNS, Timestamp: time.Unix(100, 0)})
		}
	}
	return m
}
func BenchmarkMemoryCleanup(b *testing.B) {
	m := perfMemory(100, 1000)
	cutoff := time.Unix(50, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if n := m.EvictInteractionsBefore(cutoff); n != 0 {
			b.Fatal(n)
		}
	}
}
func BenchmarkMemoryStats(b *testing.B) {
	m := perfMemory(100, 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if s := m.Stats(); s.InteractionsDNS != 100000 {
			b.Fatal(s)
		}
	}
}

// An operation is a 10,000-entry burst followed by periodic cleanup. The optional
// setter allows the identical benchmark to compile on the baseline revision.
func BenchmarkMemoryBurst(b *testing.B) {
	m := NewMemoryManager(perfIDs())
	if bounded, ok := any(m).(interface{ SetMaxPerHook(int) }); ok {
		bounded.SetMaxPerHook(1000)
	}
	id := m.CreateHook("example.test", CreateOptions{}).ID
	var retained int
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < 10000; j++ {
			m.AddInteraction(id, &Interaction{Type: InteractionTypeHTTP, Data: map[string]any{"body": strings.Repeat("x", 4096)}})
		}
		retained = len(m.interactions[id])
		m.EnforcePerHookLimit(1000)
		m.PollInteractions(id)
	}
	b.ReportMetric(float64(retained), "retained/op")
	b.ReportMetric(float64(retained*4096), "body-bytes/op")
}
func BenchmarkMemoryAdd(b *testing.B) {
	m := NewMemoryManager(perfIDs())
	if bounded, ok := any(m).(interface{ SetMaxPerHook(int) }); ok {
		bounded.SetMaxPerHook(1000)
	}
	id := m.CreateHook("example.test", CreateOptions{}).ID
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.AddInteraction(id, &Interaction{Type: InteractionTypeDNS})
		if i%1000 == 999 {
			m.PollInteractions(id)
		}
	}
}
func perfSQLite(b *testing.B) *SQLiteManager {
	b.Helper()
	m, err := NewSQLiteManager(filepath.Join(b.TempDir(), "perf.db"), perfIDs(), 65536, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { m.Close() })
	return m
}

// Fixture construction is outside the measured region.
func perfSeed(b *testing.B, m *SQLiteManager, ids []string, entries, bodySize int) {
	b.Helper()
	data, _ := json.Marshal(map[string]any{"body": strings.Repeat("x", bodySize)})
	tx, err := m.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO interactions (id, hook_id, type, timestamp, data, seq) VALUES (?, ?, 'http', 100, ?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()
	for _, id := range ids {
		for i := 1; i <= entries; i++ {
			if _, err := stmt.Exec(fmt.Sprintf("%s-%d", id, i), id, string(data), i); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := tx.Exec(`UPDATE hooks SET last_seq = ? WHERE id = ?`, entries, id); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
}
func BenchmarkSQLiteStats(b *testing.B) {
	m := perfSQLite(b)
	id := m.CreateHook("example.test", CreateOptions{}).ID
	perfSeed(b, m, []string{id}, 10000, 128)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if s := m.Stats(); s.InteractionsHTTP != 10000 {
			b.Fatal(s)
		}
	}
}
func BenchmarkSQLiteAdd(b *testing.B) {
	m := perfSQLite(b)
	id := m.CreateHook("example.test", CreateOptions{}).ID
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it := &Interaction{ID: fmt.Sprintf("entry-%d", i), Type: InteractionTypeDNS, Timestamp: time.Unix(100, 0), Data: map[string]any{"qtype": "A"}}
		m.AddInteraction(id, it)
		if it.Seq == 0 {
			b.Fatal("insert failed")
		}
	}
}

// Separate hooks keep read sizes stable. writer-ns/op is mean capture latency
// under concurrent cursor reads, not a latency percentile.
func BenchmarkSQLiteMixed(b *testing.B) {
	m := perfSQLite(b)
	readID := m.CreateHook("example.test", CreateOptions{}).ID
	writeID := m.CreateHook("example.test", CreateOptions{}).ID
	perfSeed(b, m, []string{readID}, 100, 4096)
	var seq, writes, nanos atomic.Int64
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	b.ReportAllocs()
	b.ResetTimer()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			it := &Interaction{ID: fmt.Sprintf("write-%d", seq.Add(1)), Type: InteractionTypeDNS}
			start := time.Now()
			m.AddInteraction(writeID, it)
			nanos.Add(time.Since(start).Nanoseconds())
			writes.Add(1)
			if it.Seq == 0 {
				b.Error("insert failed")
				return
			}
		}
	}()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			result, err := m.ReadInteractions(readID, 0)
			if err != nil || len(result.Interactions) != 100 {
				b.Errorf("read: %d, %v", len(result.Interactions), err)
				return
			}
		}
	})
	close(stop)
	wg.Wait()
	b.StopTimer()
	if writes.Load() > 0 {
		b.ReportMetric(float64(nanos.Load())/float64(writes.Load()), "writer-ns/op")
	}
}
func BenchmarkCompositePollBatch(b *testing.B) {
	for _, durable := range []bool{false, true} {
		b.Run(fmt.Sprintf("durable=%t", durable), func(b *testing.B) {
			mem := NewMemoryManager(perfIDs())
			var disk *SQLiteManager
			if durable {
				disk = perfSQLite(b)
			}
			m := NewCompositeManager(mem, disk, time.Hour)
			ids := make([]string, 100)
			for i := range ids {
				ttl := time.Minute
				if durable {
					ttl = 2 * time.Hour
				}
				ids[i] = m.CreateHook("example.test", CreateOptions{TTL: ttl}).ID
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				if durable {
					perfSeed(b, disk, ids, 1, 128)
				} else {
					for _, id := range ids {
						m.AddInteraction(id, &Interaction{Type: InteractionTypeDNS})
					}
				}
				b.StartTimer()
				result := m.PollInteractionsBatch(ids)
				if len(result) != len(ids) {
					b.Fatal(len(result))
				}
				for _, v := range result {
					if v.Error != "" || len(v.Interactions) != 1 {
						b.Fatal(v)
					}
				}
			}
		})
	}
}
