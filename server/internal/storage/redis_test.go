package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisTestAddrEnv names the server the Redis tests run against, e.g.
// "localhost:6379". The tests are skipped when it is unset.
const redisTestAddrEnv = "HOOKD_TEST_REDIS_ADDR"

// newTestRedis returns a manager on a fresh key prefix, so tests can share a
// server, and deletes the prefix afterwards.
func newTestRedis(t *testing.T, threshold time.Duration) *RedisManager {
	return newTestRedisWith(t, RedisOptions{EphemeralThreshold: threshold})
}

func newTestRedisWith(t *testing.T, opts RedisOptions) *RedisManager {
	t.Helper()
	addr := os.Getenv(redisTestAddrEnv)
	if addr == "" {
		t.Skip(redisTestAddrEnv + " is not set")
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	if opts.KeyPrefix == "" {
		opts.KeyPrefix = "hookdtest:" + hex.EncodeToString(suffix) + ":"
	}
	if opts.IDGenerator == nil {
		opts.IDGenerator = perfIDs()
	}
	if opts.MaxBodyBytes == 0 {
		opts.MaxBodyBytes = 1024
	}
	opts.LongLivedEnabled = true
	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	client := redis.NewClient(&redis.Options{Addr: addr})
	m, err := NewRedisManager(context.Background(), client, opts)
	if err != nil {
		client.Close()
		t.Fatalf("NewRedisManager: %v", err)
	}
	t.Cleanup(func() {
		m.Close()
		deletePrefix(t, addr, opts.KeyPrefix)
	})
	return m
}

// deletePrefix uses its own client: a test may have closed the manager's.
func deletePrefix(t *testing.T, addr, prefix string) {
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()
	ctx := context.Background()
	iter := client.Scan(ctx, 0, prefix+"*", 500).Iterator()
	for iter.Next(ctx) {
		client.Del(ctx, iter.Val())
	}
	if err := iter.Err(); err != nil {
		t.Logf("cleanup of %s failed: %v", prefix, err)
	}
}

func TestRedis_RejectsEmptyPrefix(t *testing.T) {
	_, err := NewRedisManager(context.Background(), redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}), RedisOptions{})
	if err == nil || !strings.Contains(err.Error(), "prefix") {
		t.Errorf("expected a prefix error, got %v", err)
	}
}

func TestRedis_UnreachableServer(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	defer client.Close()
	if _, err := NewRedisManager(context.Background(), client, RedisOptions{KeyPrefix: "x:"}); err == nil {
		t.Error("expected an error for an unreachable server")
	}
}

func TestRedis_PrefixesAreIsolated(t *testing.T) {
	a := newTestRedis(t, ephemeralTTL)
	b := newTestRedisWith(t, RedisOptions{EphemeralThreshold: ephemeralTTL, IDGenerator: func() string { return "id-1" }})

	hook := a.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	if b.Has(hook.ID) {
		t.Error("a hook leaked into another prefix")
	}
	if s := b.Stats(); s.HooksActive != 0 {
		t.Errorf("stats leaked into another prefix: %+v", s)
	}
}

// Two managers on one prefix are two instances behind a load balancer.
func TestRedis_InstancesShareState(t *testing.T) {
	a := newTestRedis(t, ephemeralTTL)
	b := newTestRedisWith(t, RedisOptions{KeyPrefix: a.prefix, EphemeralThreshold: ephemeralTTL})

	hook := a.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	if !b.Has(hook.ID) {
		t.Fatal("a hook created on one instance must be visible on the other")
	}
	b.AddInteraction(hook.ID, DNSInteraction("i1", "1.2.3.4", "q", "A"))
	if got := a.PollInteractions(hook.ID); len(got) != 1 || got[0].Seq != 1 {
		t.Errorf("expected the capture on the other instance, got %+v", got)
	}
}

func TestRedis_ConcurrentPollsNeverDeliverTwice(t *testing.T) {
	a := newTestRedis(t, ephemeralTTL)
	b := newTestRedisWith(t, RedisOptions{KeyPrefix: a.prefix, EphemeralThreshold: ephemeralTTL})
	hook := a.CreateHook("example.com", CreateOptions{TTL: time.Hour})

	const total = 300
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < total; i++ {
			a.AddInteraction(hook.ID, DNSInteraction(fmt.Sprintf("i%d", i), "1.2.3.4", "q", "A"))
		}
	}()

	seen := map[int64]bool{}
	results := make(chan []*Interaction)
	for _, m := range []*RedisManager{a, b} {
		go func(m *RedisManager) {
			var got []*Interaction
			for {
				select {
				case <-done:
					results <- append(got, m.PollInteractions(hook.ID)...)
					return
				default:
					got = append(got, m.PollInteractions(hook.ID)...)
				}
			}
		}(m)
	}
	for range 2 {
		for _, it := range <-results {
			if seen[it.Seq] {
				t.Fatalf("seq %d delivered twice", it.Seq)
			}
			seen[it.Seq] = true
		}
	}
	// A final drain picks up anything captured after both pollers stopped.
	for _, it := range a.PollInteractions(hook.ID) {
		seen[it.Seq] = true
	}
	if len(seen) != total {
		t.Errorf("expected %d distinct interactions, got %d", total, len(seen))
	}
}

func TestRedis_InsertLimitReportsDropsOnce(t *testing.T) {
	m := newTestRedisWith(t, RedisOptions{EphemeralThreshold: ephemeralTTL, MaxPerHook: 3})
	hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	addDNS(m, hook.ID, 5)

	read, _ := m.ReadInteractions(hook.ID, 0)
	if got := seqs(read.Interactions); fmt.Sprint(got) != "[3 4 5]" || read.DroppedThrough != 2 {
		t.Errorf("expected seqs [3 4 5] dropped through 2, got %v through %d", got, read.DroppedThrough)
	}
	if n := m.EnforcePerHookLimit(3); n != 2 {
		t.Errorf("expected the 2 insert-time drops to be reported, got %d", n)
	}
	if n := m.EnforcePerHookLimit(3); n != 0 {
		t.Errorf("drops must be reported once, got %d", n)
	}
	if s := m.Stats(); s.InteractionsTotal != 3 {
		t.Errorf("expected 3 pending, got %d", s.InteractionsTotal)
	}
}

func TestRedis_InteractionTTLEvictsEphemeralOnly(t *testing.T) {
	m := newTestRedis(t, ephemeralTTL)
	eph := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	ll, err := m.CreateLongLivedHook("example.com", CreateOptions{TTL: 2 * ephemeralTTL}, 0)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, id := range []string{eph.ID, ll.ID} {
		for i, age := range []time.Duration{3 * time.Hour, 2 * time.Hour, time.Minute} {
			it := DNSInteraction(fmt.Sprintf("%s-%d", id, i), "1.2.3.4", "q", "A")
			it.Timestamp = now.Add(-age)
			m.AddInteraction(id, it)
		}
	}

	if n := m.EvictInteractionsBefore(now.Add(-time.Hour)); n != 2 {
		t.Errorf("expected the 2 old ephemeral interactions evicted, got %d", n)
	}
	read, _ := m.ReadInteractions(eph.ID, 0)
	if got := seqs(read.Interactions); fmt.Sprint(got) != "[3]" || read.DroppedThrough != 2 {
		t.Errorf("expected seq [3] dropped through 2, got %v through %d", got, read.DroppedThrough)
	}
	if read, _ := m.ReadInteractions(ll.ID, 0); len(read.Interactions) != 3 {
		t.Errorf("long-lived interactions must not age out, got %d", len(read.Interactions))
	}
	if n := m.EvictInteractionsBefore(now.Add(-time.Hour)); n != 0 {
		t.Errorf("a second pass has nothing left to evict, got %d", n)
	}
	if s := m.Stats(); s.InteractionsTotal != 4 {
		t.Errorf("expected 4 pending, got %d", s.InteractionsTotal)
	}
}

func TestRedis_LongLivedBodiesAreTruncated(t *testing.T) {
	m := newTestRedisWith(t, RedisOptions{EphemeralThreshold: ephemeralTTL, MaxBodyBytes: 8})
	eph := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	ll, _ := m.CreateLongLivedHook("example.com", CreateOptions{TTL: 2 * ephemeralTTL}, 0)

	body := "0123456789abcdef"
	for _, id := range []string{eph.ID, ll.ID} {
		m.AddInteraction(id, HTTPInteraction("i-"+id, "1.2.3.4", "POST", "/", nil, body))
	}

	if got := m.PollInteractions(eph.ID); len(got) != 1 || got[0].Data["body"] != body || got[0].Data["truncated"] != nil {
		t.Errorf("ephemeral bodies are kept whole, got %+v", got)
	}
	if got := m.PollInteractions(ll.ID); len(got) != 1 || got[0].Data["body"] != "01234567" || got[0].Data["truncated"] != true {
		t.Errorf("long-lived bodies are truncated and flagged, got %+v", got)
	}
}

func TestRedis_LongLivedDisabled(t *testing.T) {
	m := newTestRedis(t, ephemeralTTL)
	m.longLivedEnabled = false
	if _, err := m.CreateLongLivedHook("example.com", CreateOptions{TTL: 2 * ephemeralTTL}, 0); err == nil {
		t.Error("expected an error with long-lived hooks disabled")
	}
	hook := m.CreateHook("example.com", CreateOptions{TTL: 2 * ephemeralTTL})
	if !m.Has(hook.ID) || m.LongLivedCount() != 0 {
		t.Error("with long-lived hooks disabled, a long TTL makes an ephemeral hook")
	}
}

func TestRedis_HookCollisionRetries(t *testing.T) {
	ids := []string{"dup", "dup", "fresh"}
	m := newTestRedisWith(t, RedisOptions{EphemeralThreshold: ephemeralTTL, IDGenerator: func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	}})
	m.CreateHook("example.com", CreateOptions{})
	if hook := m.CreateHook("example.com", CreateOptions{}); hook.ID != "fresh" {
		t.Errorf("expected a retry with a new id, got %q", hook.ID)
	}
	if s := m.Stats(); s.HooksActive != 2 {
		t.Errorf("a collision must not count a hook, got %d", s.HooksActive)
	}
}

func TestRedis_EvictionLease(t *testing.T) {
	a := newTestRedis(t, ephemeralTTL)
	b := newTestRedisWith(t, RedisOptions{KeyPrefix: a.prefix, EphemeralThreshold: ephemeralTTL})

	if !a.HoldEvictionLease(time.Second) {
		t.Fatal("the first instance should take the lease")
	}
	if b.HoldEvictionLease(time.Second) {
		t.Fatal("a held lease must not be shared")
	}
	if !a.HoldEvictionLease(150 * time.Millisecond) {
		t.Fatal("the holder should renew its lease")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !b.HoldEvictionLease(time.Second) {
		if time.Now().After(deadline) {
			t.Fatal("an expired lease should pass to another instance")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRedis_ScriptsSurviveFlush(t *testing.T) {
	m := newTestRedis(t, ephemeralTTL)
	hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	// A failover or restart empties the script cache.
	if err := m.client.ScriptFlush(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	addDNS(m, hook.ID, 1)
	if got := m.PollInteractions(hook.ID); len(got) != 1 {
		t.Errorf("expected scripts to reload after a flush, got %d", len(got))
	}
}

func TestRedis_StorageErrorsDegrade(t *testing.T) {
	m := newTestRedis(t, ephemeralTTL)
	hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	m.client.Close()

	if m.Has(hook.ID) {
		t.Error("Has must read as false when the server is unreachable")
	}
	it := DNSInteraction("i", "1.2.3.4", "q", "A")
	m.AddInteraction(hook.ID, it)
	if it.Seq != 0 {
		t.Error("a failed capture must not get a seq")
	}
	if res := m.PollInteractionsBatch([]string{hook.ID})[hook.ID]; res == nil || res.Error != "storage error" {
		t.Errorf("expected a storage error result, got %+v", res)
	}
	if _, err := m.ReadInteractions(hook.ID, 0); err == nil || err == ErrHookNotFound {
		t.Errorf("expected a storage error on read, got %v", err)
	}
	if _, err := m.LongLivedHooks(); err == nil {
		t.Error("expected a storage error when listing hooks")
	}
}
