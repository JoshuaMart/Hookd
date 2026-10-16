package certstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// newTestRedis returns a store on a fresh prefix of HOOKD_TEST_REDIS_ADDR and
// deletes the prefix afterwards; prefix lets a second store share the first's.
func newTestRedis(t *testing.T, prefix string) *Redis {
	t.Helper()
	addr := os.Getenv("HOOKD_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("HOOKD_TEST_REDIS_ADDR is not set")
	}
	if prefix == "" {
		b := make([]byte, 6)
		rand.Read(b)
		prefix = "hookdtest:" + hex.EncodeToString(b) + ":"
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() {
		ctx := context.Background()
		if keys, _ := client.Keys(ctx, prefix+"*").Result(); len(keys) > 0 {
			client.Del(ctx, keys...)
		}
		client.Close()
	})
	s := NewRedis(client, prefix)
	s.pollInterval = 10 * time.Millisecond
	return s
}

func TestRedis_StoreLoadStat(t *testing.T) {
	s := newTestRedis(t, "")
	ctx := context.Background()

	if _, err := s.Load(ctx, "certificates/a/a.crt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected fs.ErrNotExist for a missing key, got %v", err)
	}
	before := time.Now()
	if err := s.Store(ctx, "certificates/a/a.crt", []byte("cert")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx, "certificates/a/a.crt")
	if err != nil || string(got) != "cert" {
		t.Fatalf("Load = %q, %v", got, err)
	}

	info, err := s.Stat(ctx, "certificates/a/a.crt")
	if err != nil || !info.IsTerminal || info.Size != 4 || info.Modified.Before(before.Add(-time.Second)) {
		t.Errorf("unexpected key info %+v, %v", info, err)
	}
	if info, err := s.Stat(ctx, "certificates/a"); err != nil || info.IsTerminal {
		t.Errorf("a prefix of keys is a directory, got %+v, %v", info, err)
	}
	if _, err := s.Stat(ctx, "certificates/b"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected fs.ErrNotExist, got %v", err)
	}
	if !s.Exists(ctx, "certificates/a/a.crt") || !s.Exists(ctx, "certificates") || s.Exists(ctx, "certificates/a/a") {
		t.Error("Exists disagrees with Stat")
	}
}

func TestRedis_ListAndDelete(t *testing.T) {
	s := newTestRedis(t, "")
	ctx := context.Background()
	for _, k := range []string{"acme/acct.json", "certificates/a/a.crt", "certificates/a/a.key", "certificates/b/b.crt"} {
		if err := s.Store(ctx, k, []byte(k)); err != nil {
			t.Fatal(err)
		}
	}

	shallow, err := s.List(ctx, "certificates", false)
	if err != nil || fmt.Sprint(shallow) != "[certificates/a certificates/b]" {
		t.Errorf("non-recursive List = %v, %v", shallow, err)
	}
	deep, _ := s.List(ctx, "certificates", true)
	if fmt.Sprint(deep) != "[certificates/a certificates/a/a.crt certificates/a/a.key certificates/b certificates/b/b.crt]" {
		t.Errorf("recursive List = %v", deep)
	}
	if _, err := s.List(ctx, "nothing", true); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected fs.ErrNotExist for an empty listing, got %v", err)
	}

	// Deleting a directory removes everything under it, and only that.
	if err := s.Delete(ctx, "certificates/a"); err != nil {
		t.Fatal(err)
	}
	if s.Exists(ctx, "certificates/a/a.key") || !s.Exists(ctx, "certificates/b/b.crt") || !s.Exists(ctx, "acme/acct.json") {
		t.Error("directory delete removed the wrong keys")
	}
	if err := s.Delete(ctx, "missing"); err != nil {
		t.Errorf("deleting a missing key is not an error, got %v", err)
	}
}

// Two stores on one prefix are two instances: only one may hold a lock.
func TestRedis_LockIsExclusiveAcrossInstances(t *testing.T) {
	a := newTestRedis(t, "")
	b := newTestRedis(t, a.prefix[:len(a.prefix)-len("certs:")])
	ctx := context.Background()

	var inside, overlaps atomic.Int32
	var wg sync.WaitGroup
	for _, s := range []*Redis{a, b, a, b} {
		wg.Add(1)
		go func(s *Redis) {
			defer wg.Done()
			if err := s.Lock(ctx, "issue_cert_example.com"); err != nil {
				t.Error(err)
				return
			}
			if inside.Add(1) > 1 {
				overlaps.Add(1)
			}
			time.Sleep(30 * time.Millisecond)
			inside.Add(-1)
			if err := s.Unlock(ctx, "issue_cert_example.com"); err != nil {
				t.Error(err)
			}
		}(s)
	}
	wg.Wait()
	if overlaps.Load() != 0 {
		t.Errorf("the lock was held twice at once %d times", overlaps.Load())
	}
}

func TestRedis_LockRenewsAndExpires(t *testing.T) {
	a := newTestRedis(t, "")
	b := newTestRedis(t, a.prefix[:len(a.prefix)-len("certs:")])
	a.lockTTL = 150 * time.Millisecond
	ctx := context.Background()

	if err := a.Lock(ctx, "l"); err != nil {
		t.Fatal(err)
	}
	// Well past the TTL, the renewal must still keep b out.
	waitCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := b.Lock(waitCtx, "l"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a renewed lock must not be taken, got %v", err)
	}

	// A holder that dies stops renewing: the lock frees itself.
	a.mu.Lock()
	held := a.locks["l"]
	a.mu.Unlock()
	held.stop()
	<-held.done
	waitCtx, cancel = context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := b.Lock(waitCtx, "l"); err != nil {
		t.Fatalf("an abandoned lock should expire, got %v", err)
	}

	// The late unlock from the old holder must not release b's lock.
	if err := a.Unlock(ctx, "l"); err != nil {
		t.Fatal(err)
	}
	if held, _ := b.client.Exists(ctx, b.lockKey("l")).Result(); held != 1 {
		t.Error("a stale unlock released another instance's lock")
	}
	if err := b.Unlock(ctx, "l"); err != nil {
		t.Fatal(err)
	}
}

func TestRedis_UnlockWithoutLock(t *testing.T) {
	s := NewRedis(redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}), "x:")
	if err := s.Unlock(context.Background(), "never"); err == nil {
		t.Error("expected an error when unlocking a lock that is not held")
	}
}

func TestRedis_UnlockWithCancelledContext(t *testing.T) {
	s := newTestRedis(t, "")
	if err := s.Lock(context.Background(), "l"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Unlock(ctx, "l"); err != nil {
		t.Fatalf("a cancelled context must not prevent the release: %v", err)
	}
	if held, _ := s.client.Exists(context.Background(), s.lockKey("l")).Result(); held != 0 {
		t.Error("the lock is still held")
	}
}
