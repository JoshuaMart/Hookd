package storage

import (
	"sync"
	"time"
)

// hasCache remembers hook IDs known to exist, so most captures skip the
// EXISTS round-trip. It never remembers absence: a hook just created on
// another instance must capture from its first interaction, which is often
// the one that matters. A stale entry for a deleted hook only costs a script
// call that finds the hook gone.
//
// Two generations bound its size without per-entry bookkeeping: a hit in the
// previous generation moves to the current one, and a full current
// generation replaces the previous one.
type hasCache struct {
	mu        sync.Mutex
	ttl       time.Duration
	half      int
	cur, prev map[string]time.Time // id -> expiry
	now       func() time.Time
}

// newHasCache returns nil, a disabled cache, when size or ttl is not positive.
func newHasCache(size int, ttl time.Duration) *hasCache {
	if size <= 0 || ttl <= 0 {
		return nil
	}
	half := max(size/2, 1)
	return &hasCache{ttl: ttl, half: half, cur: make(map[string]time.Time, half), now: time.Now}
}

func (c *hasCache) has(id string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if exp, ok := c.cur[id]; ok {
		if now.Before(exp) {
			return true
		}
		delete(c.cur, id)
		return false
	}
	if exp, ok := c.prev[id]; ok {
		delete(c.prev, id)
		if now.Before(exp) {
			c.put(id, exp)
			return true
		}
	}
	return false
}

func (c *hasCache) add(id string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.put(id, c.now().Add(c.ttl))
}

// put requires c.mu.
func (c *hasCache) put(id string, exp time.Time) {
	if len(c.cur) >= c.half {
		c.prev, c.cur = c.cur, make(map[string]time.Time, c.half)
	}
	c.cur[id] = exp
}

// forget drops hooks this instance deleted; other instances keep their
// entries until the TTL ends.
func (c *hasCache) forget(ids []string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		delete(c.cur, id)
		delete(c.prev, id)
	}
}

func (c *hasCache) len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.cur) + len(c.prev)
}
