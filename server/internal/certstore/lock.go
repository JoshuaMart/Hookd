package certstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type heldLock struct {
	token string
	stop  context.CancelFunc
	done  chan struct{}
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
