// Package certstore keeps CertMagic's certificates, keys and locks in Redis,
// so instances behind a load balancer share one certificate and only one of
// them talks to the CA at a time.
package certstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/redis/go-redis/v9"
)

// Redis implements certmagic.Storage. Values live in one hash and their
// modification times in another, both keyed by the CertMagic key: there are
// only a few dozen keys, and HKEYS doubles as the listing.
type Redis struct {
	client redis.UniversalClient
	prefix string

	// lockTTL is how long a lock survives an instance that died holding it;
	// a held lock is renewed every lockTTL/3.
	lockTTL      time.Duration
	pollInterval time.Duration

	mu    sync.Mutex
	locks map[string]*heldLock
}

type heldLock struct {
	token string
	stop  context.CancelFunc
	done  chan struct{}
}

var _ certmagic.Storage = (*Redis)(nil)

// NewRedis stores everything under prefix + "certs:".
func NewRedis(client redis.UniversalClient, prefix string) *Redis {
	return &Redis{
		client:       client,
		prefix:       prefix + "certs:",
		lockTTL:      time.Minute,
		pollInterval: time.Second,
		locks:        map[string]*heldLock{},
	}
}

func (r *Redis) dataKey() string            { return r.prefix + "data" }
func (r *Redis) mtimeKey() string           { return r.prefix + "mtime" }
func (r *Redis) lockKey(name string) string { return r.prefix + "lock:" + name }

// Store writes a value and its modification time together.
func (r *Redis) Store(ctx context.Context, key string, value []byte) error {
	_, err := r.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.HSet(ctx, r.dataKey(), key, value)
		pipe.HSet(ctx, r.mtimeKey(), key, time.Now().UnixNano())
		return nil
	})
	return err
}

// Load returns fs.ErrNotExist for a missing key, as CertMagic expects.
func (r *Redis) Load(ctx context.Context, key string) ([]byte, error) {
	value, err := r.client.HGet(ctx, r.dataKey(), key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, fs.ErrNotExist
	}
	return value, err
}

// Delete removes a key, or a "directory" with every key under it.
func (r *Redis) Delete(ctx context.Context, key string) error {
	keys, err := r.keys(ctx)
	if err != nil {
		return err
	}
	var doomed []string
	for _, k := range keys {
		if k == key || strings.HasPrefix(k, key+"/") {
			doomed = append(doomed, k)
		}
	}
	if len(doomed) == 0 {
		return nil
	}
	_, err = r.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.HDel(ctx, r.dataKey(), doomed...)
		pipe.HDel(ctx, r.mtimeKey(), doomed...)
		return nil
	})
	return err
}

// Exists reports a key or a non-empty "directory".
func (r *Redis) Exists(ctx context.Context, key string) bool {
	_, err := r.Stat(ctx, key)
	return err == nil
}

// List returns the keys under prefix, or only its direct children when not
// recursive, like certmagic.FileStorage.
func (r *Redis) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	keys, err := r.keys(ctx)
	if err != nil {
		return nil, err
	}
	base := strings.TrimSuffix(prefix, "/")
	seen := map[string]bool{}
	var out []string
	for _, k := range keys {
		rest := k
		if base != "" {
			var ok bool
			if rest, ok = strings.CutPrefix(k, base+"/"); !ok {
				continue
			}
		}
		if !recursive {
			rest, _, _ = strings.Cut(rest, "/")
		}
		child := rest
		if base != "" {
			child = base + "/" + rest
		}
		if !seen[child] {
			seen[child] = true
			out = append(out, child)
		}
		// A recursive listing also names the intermediate "directories".
		if recursive {
			for dir := child; ; {
				i := strings.LastIndex(dir, "/")
				if i <= len(base) {
					break
				}
				dir = dir[:i]
				if !seen[dir] {
					seen[dir] = true
					out = append(out, dir)
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, fs.ErrNotExist
	}
	sort.Strings(out)
	return out, nil
}

// Stat describes a key, or a "directory" that prefixes other keys.
func (r *Redis) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	cmds, err := r.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.HStrLen(ctx, r.dataKey(), key)
		pipe.HGet(ctx, r.mtimeKey(), key)
		pipe.HExists(ctx, r.dataKey(), key)
		return nil
	})
	if err != nil && !errors.Is(err, redis.Nil) {
		return certmagic.KeyInfo{}, err
	}
	if cmds[2].(*redis.BoolCmd).Val() {
		nanos, _ := strconv.ParseInt(cmds[1].(*redis.StringCmd).Val(), 10, 64)
		return certmagic.KeyInfo{
			Key:        key,
			Modified:   time.Unix(0, nanos),
			Size:       cmds[0].(*redis.IntCmd).Val(),
			IsTerminal: true,
		}, nil
	}
	keys, err := r.keys(ctx)
	if err != nil {
		return certmagic.KeyInfo{}, err
	}
	for _, k := range keys {
		if strings.HasPrefix(k, strings.TrimSuffix(key, "/")+"/") {
			return certmagic.KeyInfo{Key: key, IsTerminal: false}, nil
		}
	}
	return certmagic.KeyInfo{}, fs.ErrNotExist
}

func (r *Redis) keys(ctx context.Context) ([]string, error) {
	return r.client.HKeys(ctx, r.dataKey()).Result()
}

// Lock blocks until this process holds the named lock or ctx ends. The lock
// is renewed until Unlock, and expires on its own if the process dies.
func (r *Redis) Lock(ctx context.Context, name string) error {
	token, err := newToken()
	if err != nil {
		return err
	}
	for {
		ok, err := r.client.SetNX(ctx, r.lockKey(name), token, r.lockTTL).Result()
		if err != nil {
			return fmt.Errorf("acquire lock %s: %w", name, err)
		}
		if ok {
			r.hold(name, token)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.pollInterval):
		}
	}
}

// hold records the lock and keeps renewing it in the background.
func (r *Redis) hold(name, token string) {
	ctx, stop := context.WithCancel(context.Background())
	held := &heldLock{token: token, stop: stop, done: make(chan struct{})}
	r.mu.Lock()
	r.locks[name] = held
	r.mu.Unlock()

	go func() {
		defer close(held.done)
		ticker := time.NewTicker(r.lockTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewLockScript.Run(ctx, r.client, []string{r.lockKey(name)}, token, r.lockTTL.Milliseconds())
			}
		}
	}()
}

// Unlock releases a lock this process holds, and only if it still holds it:
// an expired lock may already belong to another instance.
func (r *Redis) Unlock(ctx context.Context, name string) error {
	r.mu.Lock()
	held, ok := r.locks[name]
	delete(r.locks, name)
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("lock %s is not held", name)
	}
	held.stop()
	<-held.done
	return unlockScript.Run(ctx, r.client, []string{r.lockKey(name)}, held.token).Err()
}

func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate lock token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

var (
	renewLockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0`)

	unlockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`)
)
