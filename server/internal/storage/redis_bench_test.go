package storage

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

// The capture path against a real server: one lookup, then one write.
// Run with HOOKD_TEST_REDIS_ADDR set, e.g.
//   go test -run '^$' -bench Redis ./internal/storage/

func BenchmarkRedisHas(b *testing.B) {
	for _, bc := range []struct {
		name string
		size int
	}{{"uncached", 0}, {"cached", 50000}} {
		b.Run(bc.name, func(b *testing.B) {
			m := newTestRedisWith(b, RedisOptions{EphemeralThreshold: ephemeralTTL, HasCacheSize: bc.size, HasCacheTTL: time.Minute})
			hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if !m.Has(hook.ID) {
						b.Error("hook not found")
					}
				}
			})
		})
	}
}

// BenchmarkRedisCapture compares the sustained write rate of parallel
// synchronous captures with the background writer's. The async producer waits
// for room in the queue, so ns/op is the writer's throughput, not the cost of
// queueing, and nothing is dropped.
func BenchmarkRedisCapture(b *testing.B) {
	b.Run("sync", func(b *testing.B) {
		m := newTestRedisWith(b, RedisOptions{EphemeralThreshold: ephemeralTTL})
		hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for i := 0; pb.Next(); i++ {
				m.AddInteraction(hook.ID, DNSInteraction(fmt.Sprint(i), "1.2.3.4", "q", "A"))
			}
		})
	})
	b.Run("async", func(b *testing.B) {
		m := newTestRedisWith(b, RedisOptions{EphemeralThreshold: ephemeralTTL})
		reader := newTestRedisWith(b, RedisOptions{KeyPrefix: m.prefix, EphemeralThreshold: ephemeralTTL})
		hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for len(m.queue) >= cap(m.queue)-1 {
				runtime.Gosched()
			}
			m.RecordAsync(hook.ID, DNSInteraction(fmt.Sprint(i), "1.2.3.4", "q", "A"))
		}
		m.Close() // waits for the queue to drain
		b.StopTimer()
		if stored := reader.Stats().InteractionsTotal; stored != b.N {
			b.Errorf("expected %d stored, got %d", b.N, stored)
		}
	})
}
