package storage

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// evictBatch bounds the hooks one eviction script visits, for the same reason
// as pollChunkSize.
const evictBatch = 200

// HoldEvictionLease takes or renews the lease that lets one instance run the
// eviction passes for all of them.
func (m *RedisManager) HoldEvictionLease(ttl time.Duration) bool {
	held, err := m.run(context.Background(), leaseScript, m.owner, ttl.Milliseconds()).Int64()
	if err != nil {
		m.logFailure("eviction lease", err)
		return false
	}
	return held == 1
}

// EvictInteractionsBefore removes ephemeral interactions older than cutoff.
// Long-lived interactions are kept until polled or until their hook expires,
// as with SQLite.
func (m *RedisManager) EvictInteractionsBefore(cutoff time.Time) int {
	total := 0
	for {
		reply, err := m.run(context.Background(), evictBeforeScript, micros(cutoff), evictBatch).Int64Slice()
		if err != nil || len(reply) != 2 {
			m.logFailure("interaction ttl eviction", err)
			return total
		}
		total += int(reply[1])
		if reply[0] < evictBatch {
			return total
		}
	}
}

// EvictExpiredHooks removes hooks whose ExpiresAt is before now, with their
// interactions, and returns how many were removed.
func (m *RedisManager) EvictExpiredHooks(now time.Time) int {
	total := 0
	for {
		n, err := m.run(context.Background(), evictExpiredScript, micros(now), evictBatch).Int()
		if err != nil {
			m.logFailure("hook ttl eviction", err)
			return total
		}
		total += n
		if n < evictBatch {
			return total
		}
	}
}

// EnforcePerHookLimit reports the insert-time trims since the last pass and,
// when max is below the insert-time limit, trims every hook with pending
// interactions.
func (m *RedisManager) EnforcePerHookLimit(max int) int {
	ctx := context.Background()
	total, err := m.client.GetDel(ctx, m.key("evict:limit")).Int()
	if err != nil && !errors.Is(err, redis.Nil) {
		m.logFailure("read limit evictions", err)
	}
	if m.maxPerHook > 0 && max >= m.maxPerHook {
		return total
	}
	for _, index := range []string{"ttl", "ll:active"} {
		for start := int64(0); ; start += evictBatch {
			ids, err := m.client.ZRange(ctx, m.key(index), start, start+evictBatch-1).Result()
			if err != nil {
				m.logFailure("list hooks to trim", err)
				return total
			}
			if len(ids) == 0 {
				break
			}
			args := []any{max}
			for _, id := range ids {
				args = append(args, id)
			}
			n, err := m.run(ctx, trimScript, args...).Int()
			if err != nil {
				m.logFailure("per-hook limit eviction", err)
				return total
			}
			total += n
			if len(ids) < evictBatch {
				break
			}
		}
	}
	return total
}

// EvictByMemoryPressure is a no-op: the data lives in Redis, whose own
// maxmemory setting bounds it.
func (m *RedisManager) EvictByMemoryPressure(_ int) MemoryEvictionResult {
	return MemoryEvictionResult{}
}
