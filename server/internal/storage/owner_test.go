package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Owners are SHA-256 hex digests in production; any distinct strings do here.
var (
	ownerA = strings.Repeat("a", 64)
	ownerB = strings.Repeat("b", 64)
)

func TestOwner_ForeignHookReadsAsUnknown(t *testing.T) {
	forEachStore(t, func(t *testing.T, m testManager) {
		theirs := m.CreateHook("example.com", CreateOptions{TTL: time.Hour, Owner: ownerA})
		mine := m.CreateHook("example.com", CreateOptions{TTL: time.Hour, Owner: ownerB})
		addDNS(m, theirs.ID, 2)
		addDNS(m, mine.ID, 1)

		if hook, ok := m.GetOwnedHook(OwnerScope(ownerA), theirs.ID); !ok || hook.Owner != ownerA {
			t.Fatalf("the owner must see its hook, got %+v %v", hook, ok)
		}
		if _, ok := m.GetOwnedHook(OwnerScope(ownerB), theirs.ID); ok {
			t.Error("another owner must not see the hook")
		}
		if _, err := m.ReadOwned(OwnerScope(ownerB), theirs.ID, 0); !errors.Is(err, ErrHookNotFound) {
			t.Errorf("read by another owner: want ErrHookNotFound, got %v", err)
		}
		if n, err := m.AckOwned(OwnerScope(ownerB), theirs.ID, 10); n != 0 || !errors.Is(err, ErrHookNotFound) {
			t.Errorf("ack by another owner: want ErrHookNotFound, got %d %v", n, err)
		}

		results := m.PollOwned(OwnerScope(ownerB), []string{theirs.ID, "unknown", mine.ID})
		if !sameJSON(t, results[theirs.ID], results["unknown"]) {
			t.Errorf("a foreign hook must poll like an unknown one: %+v vs %+v", results[theirs.ID], results["unknown"])
		}
		if r := results[mine.ID]; r.Error != "" || len(r.Interactions) != 1 {
			t.Errorf("own hook in the same batch: %+v", r)
		}

		// Nothing was drained or acknowledged by the foreign calls.
		read, err := m.ReadOwned(OwnerScope(ownerA), theirs.ID, 0)
		if err != nil || len(read.Interactions) != 2 {
			t.Fatalf("owner lost interactions: %v %+v", err, read)
		}
		if n, err := m.AckOwned(OwnerScope(ownerA), theirs.ID, 1); n != 1 || err != nil {
			t.Errorf("owner ack: %d %v", n, err)
		}
		if r := m.PollOwned(OwnerScope(ownerA), []string{theirs.ID})[theirs.ID]; len(r.Interactions) != 1 {
			t.Errorf("owner poll: %+v", r)
		}
		if _, ok := m.GetOwnedHook(AllOwners(), theirs.ID); !ok {
			t.Error("AllOwners() must bypass the check")
		}
	})
}

func TestOwner_HookWithoutOwnerIsHidden(t *testing.T) {
	forEachStore(t, func(t *testing.T, m testManager) {
		legacy := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
		if _, ok := m.GetOwnedHook(OwnerScope(ownerA), legacy.ID); ok {
			t.Error("a hook without owner must be hidden from owners")
		}
		if _, err := m.ReadOwned(OwnerScope(ownerA), legacy.ID, 0); !errors.Is(err, ErrHookNotFound) {
			t.Errorf("want ErrHookNotFound, got %v", err)
		}
		if _, ok := m.GetHook(legacy.ID); !ok {
			t.Error("the unscoped path must still see it")
		}
	})
}

func TestOwner_OwnerIsNeverSerialized(t *testing.T) {
	b, err := json.Marshal(HookActivity{Hook: newHook("x", "example.com", CreateOptions{Owner: ownerA})})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), ownerA) || strings.Contains(string(b), "owner") {
		t.Errorf("owner leaked into JSON: %s", b)
	}
}

func TestOwner_ListingsAreScoped(t *testing.T) {
	forEachLongLivedStore(t, func(t *testing.T, m fullManager) {
		create := func(owner string) *Hook {
			h, err := m.CreateLongLivedHook("example.com", CreateOptions{TTL: 2 * ephemeralTTL, Owner: owner}, 0)
			if err != nil {
				t.Fatal(err)
			}
			return h
		}
		a1, b1, a2, legacy := create(ownerA), create(ownerB), create(ownerA), create("")

		hooks, err := m.LongLivedHooks(OwnerScope(ownerA))
		if err != nil {
			t.Fatal(err)
		}
		if got := hookIDs(hooks); got != fmt.Sprint([]string{a1.ID, a2.ID}) {
			t.Errorf("owner A hooks, oldest first: got %s", got)
		}
		if all, _ := m.LongLivedHooks(AllOwners()); len(all) != 4 {
			t.Errorf("AllOwners() must list every hook, got %d", len(all))
		}

		at := time.Now().UTC().Add(-time.Minute)
		for i, id := range []string{a2.ID, b1.ID, a1.ID, legacy.ID} {
			it := DNSInteraction(fmt.Sprintf("i%d", i), "1.2.3.4", "q", "A")
			it.Timestamp = at.Add(time.Duration(i) * time.Second)
			m.AddInteraction(id, it)
		}
		activity := m.LongLivedActivity(OwnerScope(ownerA))
		if len(activity) != 2 || activity[0].Hook.ID != a1.ID || activity[1].Hook.ID != a2.ID {
			t.Errorf("owner A activity, latest first: got %+v", activity)
		}
		if activity := m.LongLivedActivity(OwnerScope(ownerB)); len(activity) != 1 || activity[0].Hook.ID != b1.ID {
			t.Errorf("owner B activity: got %+v", activity)
		}
		if activity := m.LongLivedActivity(OwnerScope(strings.Repeat("c", 64))); len(activity) != 0 {
			t.Errorf("an owner without hooks has no activity, got %+v", activity)
		}
	})
}

func TestOwner_EvictionClearsOwnerListing(t *testing.T) {
	forEachLongLivedStore(t, func(t *testing.T, m fullManager) {
		short, _ := m.CreateLongLivedHook("example.com", CreateOptions{TTL: 2 * ephemeralTTL, Owner: ownerA}, 0)
		long, _ := m.CreateLongLivedHook("example.com", CreateOptions{TTL: 4 * ephemeralTTL, Owner: ownerA}, 0)
		addDNS(m, short.ID, 1)

		m.EvictExpiredHooks(time.Now().Add(3 * ephemeralTTL))
		hooks, err := m.LongLivedHooks(OwnerScope(ownerA))
		if err != nil || hookIDs(hooks) != fmt.Sprint([]string{long.ID}) {
			t.Errorf("expected only %s after eviction, got %s %v", long.ID, hookIDs(hooks), err)
		}
		if len(m.LongLivedActivity(OwnerScope(ownerA))) != 0 {
			t.Error("an evicted hook is no longer active")
		}
	})
}

func hookIDs(hooks []*Hook) string {
	ids := make([]string, len(hooks))
	for i, h := range hooks {
		ids[i] = h.ID
	}
	return fmt.Sprint(ids)
}

func TestSQLite_OwnerIndexFollowsEviction(t *testing.T) {
	m := newTestSQLite(t, 1024)
	hook, _ := m.CreateLongLivedHook("example.com", CreateOptions{TTL: time.Hour, Owner: ownerA}, 0)

	var plan string
	rows, err := m.readDB.Query(`EXPLAIN QUERY PLAN SELECT `+hookColumns+` FROM hooks WHERE owner = ? ORDER BY created_at, id`, ownerA)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	rows.Close()
	if !strings.Contains(plan, "idx_hooks_owner") {
		t.Errorf("the owner listing must use its index:\n%s", plan)
	}

	m.EvictExpiredHooks(time.Now().Add(2 * time.Hour))
	var n int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM hooks WHERE owner = ?`, ownerA).Scan(&n); err != nil || n != 0 {
		t.Errorf("expected no row left for the owner, got %d %v", n, err)
	}
	if m.owns(OwnerScope(ownerA), hook.ID) {
		t.Error("the in-memory index must forget the evicted hook")
	}
}

func TestSQLite_OwnerSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.db")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := NewSQLiteManager(path, perfIDs(), 1024, logger)
	if err != nil {
		t.Fatal(err)
	}
	hook, _ := m.CreateLongLivedHook("example.com", CreateOptions{TTL: time.Hour, Owner: ownerA}, 0)
	m.Close()

	m, err = NewSQLiteManager(path, perfIDs(), 1024, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, ok := m.GetOwnedHook(OwnerScope(ownerA), hook.ID); !ok {
		t.Error("the owner must be reloaded on startup")
	}
	if _, ok := m.GetOwnedHook(OwnerScope(ownerB), hook.ID); ok {
		t.Error("another owner must stay refused after a restart")
	}
}

// A database from before owner secrets keeps its hooks, without owner.
func TestSQLite_MigratesOwnerColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE hooks (
			id TEXT PRIMARY KEY, dns TEXT NOT NULL, http TEXT NOT NULL, https TEXT NOT NULL,
			smtp TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
			metadata TEXT, last_seq INTEGER NOT NULL DEFAULT 0, dropped_through INTEGER NOT NULL DEFAULT 0
		);
		INSERT INTO hooks (id, dns, http, https, created_at, expires_at) VALUES ('old1', 'd', 'h', 's', 1, 0);
	`); err != nil {
		t.Fatal(err)
	}
	legacy.Close()

	m, err := NewSQLiteManager(path, perfIDs(), 1024, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewSQLiteManager on a legacy db: %v", err)
	}
	defer m.Close()
	if _, ok := m.GetOwnedHook(OwnerScope(ownerA), "old1"); ok {
		t.Error("a legacy hook must not belong to any owner")
	}
	if hook, ok := m.GetHook("old1"); !ok || hook.Owner != "" {
		t.Errorf("legacy hook must survive with an empty owner, got %+v %v", hook, ok)
	}
	if hooks, err := m.LongLivedHooks(OwnerScope(ownerA)); err != nil || len(hooks) != 0 {
		t.Errorf("legacy hooks are listed for no owner, got %d %v", len(hooks), err)
	}
}

func newOwnerRedisClient(t *testing.T) *redis.Client {
	client := redis.NewClient(&redis.Options{Addr: os.Getenv(redisTestAddrEnv)})
	t.Cleanup(func() { client.Close() })
	return client
}

func TestRedis_OwnerIndexFollowsEviction(t *testing.T) {
	m := newTestRedis(t, ephemeralTTL)
	client := newOwnerRedisClient(t)
	ctx := context.Background()
	hook, _ := m.CreateLongLivedHook("example.com", CreateOptions{TTL: 2 * ephemeralTTL, Owner: ownerA}, 0)

	if n := client.ZCard(ctx, m.ownerKey(ownerA)).Val(); n != 1 {
		t.Fatalf("expected the hook in the owner index, got %d", n)
	}
	if v := client.HGet(ctx, m.hookKey(hook.ID), "owner").Val(); v != ownerA {
		t.Errorf("hash owner = %q", v)
	}
	m.EvictExpiredHooks(time.Now().Add(3 * ephemeralTTL))
	if client.Exists(ctx, m.ownerKey(ownerA)).Val() != 0 {
		t.Error("the owner index must be removed with its last hook")
	}
}

func TestRedis_OwnerIndexPrunesHooksGoneBeforeEviction(t *testing.T) {
	m := newTestRedis(t, ephemeralTTL)
	client := newOwnerRedisClient(t)
	ctx := context.Background()
	gone, _ := m.CreateLongLivedHook("example.com", CreateOptions{TTL: 2 * ephemeralTTL, Owner: ownerA}, 0)
	kept, _ := m.CreateLongLivedHook("example.com", CreateOptions{TTL: 2 * ephemeralTTL, Owner: ownerA}, 0)

	// The safety-net expiry fired first: evict_expired can no longer read the owner.
	client.Del(ctx, m.hookKey(gone.ID))
	hooks, err := m.LongLivedHooks(OwnerScope(ownerA))
	if err != nil || hookIDs(hooks) != fmt.Sprint([]string{kept.ID}) {
		t.Fatalf("got %s %v", hookIDs(hooks), err)
	}
	if ids := client.ZRange(ctx, m.ownerKey(ownerA), 0, -1).Val(); fmt.Sprint(ids) != fmt.Sprint([]string{kept.ID}) {
		t.Errorf("the vanished hook must be pruned from the index, got %v", ids)
	}
}

func TestRedis_OwnerIndexExpiresWithItsLastHook(t *testing.T) {
	m := newTestRedis(t, ephemeralTTL)
	client := newOwnerRedisClient(t)
	ctx := context.Background()
	expiry := func() time.Duration { return client.PExpireTime(ctx, m.ownerKey(ownerA)).Val() }
	create := func(ttl time.Duration) *Hook {
		h, err := m.CreateLongLivedHook("example.com", CreateOptions{TTL: ttl, Owner: ownerA}, 0)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	want := func(h *Hook) time.Duration {
		return time.Duration(h.ExpiresAt.Add(expiryGrace).UnixMilli()) * time.Millisecond
	}

	mid := create(3 * ephemeralTTL)
	if got := expiry(); got != want(mid) {
		t.Errorf("index expiry = %v, want %v", got, want(mid))
	}
	create(2 * ephemeralTTL) // earlier: must not shorten it
	late := create(4 * ephemeralTTL)
	if got := expiry(); got != want(late) {
		t.Errorf("index expiry = %v, want the latest hook's %v", got, want(late))
	}
	create(0) // no expiry: the index must persist
	if ttl := client.PTTL(ctx, m.ownerKey(ownerA)).Val(); ttl != -1 {
		t.Errorf("expected a persistent index, got ttl %v", ttl)
	}
}

func TestRedis_HasCacheDoesNotBypassOwner(t *testing.T) {
	m := newTestRedisWith(t, RedisOptions{EphemeralThreshold: ephemeralTTL, HasCacheSize: 100, HasCacheTTL: time.Hour})
	hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour, Owner: ownerA})
	addDNS(m, hook.ID, 1)
	if !m.known.has(hook.ID) {
		t.Fatal("the created hook must be cached")
	}
	if _, ok := m.GetOwnedHook(OwnerScope(ownerB), hook.ID); ok {
		t.Error("a cached hook must still be checked for its owner")
	}
	if r := m.PollOwned(OwnerScope(ownerB), []string{hook.ID})[hook.ID]; r.Error != "Hook not found" {
		t.Errorf("poll by another owner: %+v", r)
	}
	if read, err := m.ReadOwned(OwnerScope(ownerA), hook.ID, 0); err != nil || len(read.Interactions) != 1 {
		t.Errorf("the owner's interaction must be intact: %+v %v", read, err)
	}
}

func TestOwner_ZeroScopeMatchesNothing(t *testing.T) {
	forEachStore(t, func(t *testing.T, m testManager) {
		legacy := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
		owned := m.CreateHook("example.com", CreateOptions{TTL: time.Hour, Owner: ownerA})
		addDNS(m, legacy.ID, 1)
		for _, scope := range []Scope{{}, OwnerScope("")} {
			for _, id := range []string{legacy.ID, owned.ID} {
				if _, ok := m.GetOwnedHook(scope, id); ok {
					t.Errorf("%+v must not see %s", scope, id)
				}
				if _, err := m.ReadOwned(scope, id, 0); !errors.Is(err, ErrHookNotFound) {
					t.Errorf("read: want ErrHookNotFound, got %v", err)
				}
				if _, err := m.AckOwned(scope, id, 1); !errors.Is(err, ErrHookNotFound) {
					t.Errorf("ack: want ErrHookNotFound, got %v", err)
				}
				if r := m.PollOwned(scope, []string{id})[id]; r.Error != "Hook not found" {
					t.Errorf("poll: %+v", r)
				}
			}
		}
		if read, _ := m.ReadInteractions(legacy.ID, 0); len(read.Interactions) != 1 {
			t.Error("refused calls must leave interactions alone")
		}
	})
}

// A separate test: a repeated subtest name would put '#' in the SQLite path.
func TestOwner_ZeroScopeListsNothing(t *testing.T) {
	forEachLongLivedStore(t, func(t *testing.T, m fullManager) {
		if _, err := m.CreateLongLivedHook("example.com", CreateOptions{TTL: 2 * ephemeralTTL}, 0); err != nil {
			t.Fatal(err)
		}
		if hooks, err := m.LongLivedHooks(Scope{}); err != nil || len(hooks) != 0 {
			t.Errorf("zero scope listed %d hooks, err %v", len(hooks), err)
		}
		if a := m.LongLivedActivity(OwnerScope("")); len(a) != 0 {
			t.Errorf("empty owner listed activity %+v", a)
		}
	})
}

// newCappedStores returns each long-lived backend with a per-owner cap.
func newCappedStores(t *testing.T, max int) map[string]fullManager {
	sqlite := newTestSQLiteWithID(t, 1024, perfIDs())
	sqlite.SetMaxPerOwner(max)
	stores := map[string]fullManager{"sqlite": sqlite}
	if os.Getenv(redisTestAddrEnv) != "" {
		stores["redis"] = newTestRedisWith(t, RedisOptions{EphemeralThreshold: ephemeralTTL, MaxLongLivedPerOwner: max})
	}
	return stores
}

func TestOwner_PerOwnerCap(t *testing.T) {
	for name, m := range newCappedStores(t, 2) {
		t.Run(name, func(t *testing.T) {
			create := func(owner string, n int, ttl time.Duration) error {
				opts := make([]CreateOptions, n)
				for i := range opts {
					opts[i] = CreateOptions{TTL: ttl, Owner: owner}
				}
				_, err := m.CreateLongLivedHooks("example.com", opts, 0)
				return err
			}
			if err := create(ownerA, 3, 2*ephemeralTTL); !errors.Is(err, ErrOwnerHookLimitReached) {
				t.Errorf("a batch over the cap: want ErrOwnerHookLimitReached, got %v", err)
			}
			if err := create(ownerA, 1, 2*ephemeralTTL); err != nil {
				t.Fatal(err)
			}
			if err := create(ownerA, 1, 4*ephemeralTTL); err != nil {
				t.Fatal(err)
			}
			if err := create(ownerA, 1, 2*ephemeralTTL); !errors.Is(err, ErrOwnerHookLimitReached) {
				t.Errorf("third hook of A: want ErrOwnerHookLimitReached, got %v", err)
			}
			if err := create(ownerB, 2, 2*ephemeralTTL); err != nil {
				t.Errorf("B has its own share: %v", err)
			}
			if err := create("", 3, 2*ephemeralTTL); err != nil {
				t.Errorf("hooks without owner are not capped per owner: %v", err)
			}
			// Eviction gives the slot back.
			m.EvictExpiredHooks(time.Now().Add(3 * ephemeralTTL))
			if err := create(ownerA, 1, 2*ephemeralTTL); err != nil {
				t.Errorf("an evicted hook must free its slot: %v", err)
			}
		})
	}
}

func TestSQLite_PerOwnerCountsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cap.db")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := NewSQLiteManager(path, perfIDs(), 1024, logger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateLongLivedHook("example.com", CreateOptions{TTL: time.Hour, Owner: ownerA}, 0); err != nil {
		t.Fatal(err)
	}
	m.Close()

	m, err = NewSQLiteManager(path, perfIDs(), 1024, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.SetMaxPerOwner(1)
	if _, err := m.CreateLongLivedHook("example.com", CreateOptions{TTL: time.Hour, Owner: ownerA}, 0); !errors.Is(err, ErrOwnerHookLimitReached) {
		t.Errorf("the count must be reloaded on startup, got %v", err)
	}
}

// A hash written before owner secrets has no owner field at all.
func TestRedis_HashWithoutOwnerFieldIsHidden(t *testing.T) {
	m := newTestRedis(t, ephemeralTTL)
	client := newOwnerRedisClient(t)
	ctx := context.Background()
	hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	addDNS(m, hook.ID, 1)
	client.HDel(ctx, m.hookKey(hook.ID), "owner")

	if _, ok := m.GetOwnedHook(OwnerScope(ownerA), hook.ID); ok {
		t.Error("get must refuse a legacy hook")
	}
	if _, err := m.ReadOwned(OwnerScope(ownerA), hook.ID, 0); !errors.Is(err, ErrHookNotFound) {
		t.Errorf("read: %v", err)
	}
	if _, err := m.AckOwned(OwnerScope(ownerA), hook.ID, 1); !errors.Is(err, ErrHookNotFound) {
		t.Errorf("ack: %v", err)
	}
	if r := m.PollOwned(OwnerScope(ownerA), []string{hook.ID})[hook.ID]; r.Error != "Hook not found" {
		t.Errorf("poll: %+v", r)
	}
	if got, ok := m.GetHook(hook.ID); !ok || got.Owner != "" {
		t.Errorf("the unscoped path must still read it: %+v %v", got, ok)
	}
	if read, _ := m.ReadInteractions(hook.ID, 0); len(read.Interactions) != 1 {
		t.Error("the legacy hook's interaction must be intact")
	}
}
