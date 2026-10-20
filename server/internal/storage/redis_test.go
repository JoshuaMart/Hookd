package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
func newTestRedis(t testing.TB, threshold time.Duration) *RedisManager {
	return newTestRedisWith(t, RedisOptions{EphemeralThreshold: threshold})
}

func newTestRedisWith(t testing.TB, opts RedisOptions) *RedisManager {
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

	client := redis.NewClient(&redis.Options{Addr: addr, ContextTimeoutEnabled: true})
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
func deletePrefix(t testing.TB, addr, prefix string) {
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
	if _, err := m.LongLivedHooks(AnyOwner); err == nil {
		t.Error("expected a storage error when listing hooks")
	}
}

func TestRedis_HasCacheNeverHidesNewHooks(t *testing.T) {
	a := newTestRedisWith(t, RedisOptions{EphemeralThreshold: ephemeralTTL, HasCacheSize: 100, HasCacheTTL: time.Hour})
	b := newTestRedisWith(t, RedisOptions{KeyPrefix: a.prefix, EphemeralThreshold: ephemeralTTL, HasCacheSize: 100, HasCacheTTL: time.Hour})

	if a.Has("id-1") {
		t.Fatal("unexpected hook")
	}
	// Absence is not cached: a hook created on another instance is seen at once.
	hook := b.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	if !a.Has(hook.ID) {
		t.Fatal("a hook created on another instance must be visible immediately")
	}

	// Existence is cached: the next lookup needs no round-trip.
	a.client.Del(context.Background(), a.hookKey(hook.ID))
	if !a.Has(hook.ID) {
		t.Error("expected the cached answer")
	}
	// The stale entry costs only a write that the script rejects.
	it := DNSInteraction("late", "1.2.3.4", "q", "A")
	a.AddInteraction(hook.ID, it)
	if it.Seq != 0 || a.Stats().InteractionsTotal != 0 {
		t.Error("a capture for a deleted hook must still be dropped")
	}
}

func TestRedis_HasCacheForgetsLocalEvictions(t *testing.T) {
	m := newTestRedisWith(t, RedisOptions{EphemeralThreshold: ephemeralTTL, HasCacheSize: 100, HasCacheTTL: time.Hour})
	hook := m.CreateHook("example.com", CreateOptions{TTL: time.Minute})
	if !m.Has(hook.ID) {
		t.Fatal("expected the new hook")
	}
	m.EvictExpiredHooks(time.Now().Add(time.Hour))
	if m.Has(hook.ID) {
		t.Error("a hook this instance evicted must leave its cache")
	}
}

func TestRedis_RecordAsyncKeepsOrderAndFlushesOnClose(t *testing.T) {
	m := newTestRedis(t, ephemeralTTL)
	reader := newTestRedisWith(t, RedisOptions{KeyPrefix: m.prefix, EphemeralThreshold: ephemeralTTL})
	hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})

	const n = 500
	for i := 0; i < n; i++ {
		m.RecordAsync(hook.ID, DNSInteraction(fmt.Sprintf("i%03d", i), "1.2.3.4", "q", "A"))
	}
	m.RecordAsync("missing", DNSInteraction("x", "1.2.3.4", "q", "A"))
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m.RecordAsync(hook.ID, DNSInteraction("after-close", "1.2.3.4", "q", "A"))

	got := reader.PollInteractions(hook.ID)
	if len(got) != n {
		t.Fatalf("Close must flush every queued capture, got %d of %d", len(got), n)
	}
	for i, it := range got {
		if it.ID != fmt.Sprintf("i%03d", i) || it.Seq != int64(i+1) {
			t.Fatalf("captures must keep their arrival order, got %s seq %d at %d", it.ID, it.Seq, i)
		}
	}
}

func TestRedis_RecordAsyncRecoversScripts(t *testing.T) {
	m := newTestRedis(t, ephemeralTTL)
	hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	if err := m.client.ScriptFlush(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	m.RecordAsync(hook.ID, DNSInteraction("i", "1.2.3.4", "q", "A"))

	deadline := time.Now().Add(2 * time.Second)
	for len(m.PollInteractions(hook.ID)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a pipelined capture must survive an empty script cache")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRedis_RecordAsyncDropsWhenFull(t *testing.T) {
	m := newTestRedis(t, ephemeralTTL)
	// Stop the writer so nothing drains, then overfill the queue.
	close(m.stop)
	<-m.done
	m.stop = make(chan struct{})
	for i := 0; i < asyncQueueSize+10; i++ {
		m.RecordAsync("h", &Interaction{ID: fmt.Sprint(i), Type: InteractionTypeDNS})
	}
	if len(m.queue) != asyncQueueSize {
		t.Errorf("the queue must stay bounded, holds %d", len(m.queue))
	}
	m.done = make(chan struct{})
	go m.writeLoop()
}

func TestRedis_CreateHookBatchIsAtomicAcrossKinds(t *testing.T) {
	m := newTestRedis(t, ephemeralTTL)
	eph, ll := CreateOptions{TTL: time.Hour}, CreateOptions{TTL: 2 * ephemeralTTL}

	hooks, err := m.CreateHookBatch("example.com", []CreateOptions{eph, ll, eph}, 2)
	if err != nil || len(hooks) != 3 {
		t.Fatalf("expected 3 hooks, got %d (%v)", len(hooks), err)
	}
	if m.LongLivedCount() != 1 || m.Stats().HooksActive != 3 {
		t.Fatalf("expected 1 long-lived of 3 hooks, got %d of %d", m.LongLivedCount(), m.Stats().HooksActive)
	}

	// Two more long-lived hooks exceed the cap: the ephemeral one is not created either.
	if _, err := m.CreateHookBatch("example.com", []CreateOptions{eph, ll, ll}, 2); !errors.Is(err, ErrHookLimitReached) {
		t.Fatalf("expected ErrHookLimitReached, got %v", err)
	}
	if n := m.Stats().HooksActive; n != 3 {
		t.Errorf("a refused batch must create nothing, got %d hooks", n)
	}

	m.client.Close()
	if _, err := m.CreateHookBatch("example.com", []CreateOptions{eph}, 0); err == nil {
		t.Error("a storage failure must be reported")
	}
}

func TestRedis_CloseDoesNotHangOnAStalledServer(t *testing.T) {
	asyncTimeout, drainTimeout = 200*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { asyncTimeout, drainTimeout = 5*time.Second, 5*time.Second })

	m := newTestRedis(t, ephemeralTTL)
	hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	admin := redis.NewClient(&redis.Options{Addr: os.Getenv(redisTestAddrEnv)})
	defer admin.Close()
	// Every other client blocks until the pause ends, like an unreachable server.
	if err := admin.Do(context.Background(), "CLIENT", "PAUSE", 3000, "ALL").Err(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		m.RecordAsync(hook.ID, DNSInteraction(fmt.Sprint(i), "1.2.3.4", "q", "A"))
	}

	start := time.Now()
	m.Close()
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("Close took %s with a stalled server", elapsed)
	}
	time.Sleep(3 * time.Second) // let the pause end before cleanup
}

func TestRedis_FailureLogsAreLimitedPerOperation(t *testing.T) {
	var buf strings.Builder
	m := &RedisManager{logger: slog.New(slog.NewTextHandler(&buf, nil))}
	for range 5 {
		m.logFailure("has", errors.New("down"))
	}
	m.logFailure("create hook", errors.New("down"))
	if n := strings.Count(buf.String(), "redis storage has failed"); n != 1 {
		t.Errorf("expected one has line, got %d", n)
	}
	if !strings.Contains(buf.String(), "redis storage create hook failed") {
		t.Error("a burst of one failure must not hide another")
	}
}

// Trimming rewrites ttl scores mid-pass; every hook must still be visited.
func TestRedis_LimitPassTrimsEveryHook(t *testing.T) {
	m := newTestRedis(t, ephemeralTTL)
	now := time.Now().UTC()
	var ids []string
	for h := 0; h < 3*evictBatch; h++ {
		hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
		ids = append(ids, hook.ID)
		for i := 0; i < 3; i++ {
			it := DNSInteraction(fmt.Sprint(i), "1.2.3.4", "q", "A")
			it.Timestamp = now.Add(time.Duration(h-i) * time.Millisecond)
			m.AddInteraction(hook.ID, it)
		}
	}
	if n := m.EnforcePerHookLimit(1); n != 2*len(ids) {
		t.Errorf("expected %d trimmed, got %d", 2*len(ids), n)
	}
	for _, id := range ids {
		if read, _ := m.ReadInteractions(id, 0); len(read.Interactions) != 1 {
			t.Fatalf("hook %s kept %d interactions", id, len(read.Interactions))
		}
	}
}

func TestRedis_FirstExistingInOneRoundTrip(t *testing.T) {
	m := newTestRedisWith(t, RedisOptions{EphemeralThreshold: ephemeralTTL, HasCacheSize: 100, HasCacheTTL: time.Hour})
	hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	other := newTestRedisWith(t, RedisOptions{KeyPrefix: m.prefix, EphemeralThreshold: ephemeralTTL, HasCacheSize: 100, HasCacheTTL: time.Hour})

	if i := other.FirstExisting("x", hook.ID); i != 1 {
		t.Errorf("expected the tail candidate, got %d", i)
	}
	if i := other.FirstExisting("x", "y"); i != -1 {
		t.Errorf("expected no match, got %d", i)
	}
	// The first label wins over a cached tail when it exists too.
	second := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	if i := other.FirstExisting(second.ID, hook.ID); i != 0 {
		t.Errorf("the first label must win, got %d", i)
	}
	if !other.known.has(hook.ID) || !other.known.has(second.ID) {
		t.Error("found ids must be cached")
	}
}
