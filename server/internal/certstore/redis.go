// Package certstore keeps CertMagic's certificates, keys and locks in Redis,
// so instances behind a load balancer share one certificate and only one of
// them talks to the CA at a time.
package certstore

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"path"
	"slices"
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
	below, err := r.keysUnder(ctx, key)
	if err != nil {
		return err
	}
	doomed := []string{key} // deleting a missing field is a no-op
	for _, rel := range below {
		doomed = append(doomed, path.Join(key, rel))
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
// recursive, like certmagic.FileStorage. A recursive listing also names the
// intermediate "directories".
func (r *Redis) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	below, err := r.keysUnder(ctx, prefix)
	if err != nil {
		return nil, err
	}
	listed := map[string]bool{}
	for _, rel := range below {
		parts := strings.Split(rel, "/")
		depth := 1
		if recursive {
			depth = len(parts)
		}
		for i := 1; i <= depth; i++ {
			listed[path.Join(prefix, strings.Join(parts[:i], "/"))] = true
		}
	}
	if len(listed) == 0 {
		return nil, fs.ErrNotExist
	}
	return slices.Sorted(maps.Keys(listed)), nil
}

// Stat describes a key, or a "directory" that prefixes other keys. Every
// Store writes a modification time, so its presence means the key exists.
func (r *Redis) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	pipe := r.client.TxPipeline()
	size := pipe.HStrLen(ctx, r.dataKey(), key)
	mtime := pipe.HGet(ctx, r.mtimeKey(), key)
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return certmagic.KeyInfo{}, err
	}
	if nanos, err := mtime.Int64(); err == nil {
		return certmagic.KeyInfo{Key: key, Modified: time.Unix(0, nanos), Size: size.Val(), IsTerminal: true}, nil
	}
	below, err := r.keysUnder(ctx, key)
	if err != nil {
		return certmagic.KeyInfo{}, err
	}
	if len(below) == 0 {
		return certmagic.KeyInfo{}, fs.ErrNotExist
	}
	return certmagic.KeyInfo{Key: key, IsTerminal: false}, nil
}

// keysUnder returns the stored keys below dir, relative to it; every key
// when dir is empty.
func (r *Redis) keysUnder(ctx context.Context, dir string) ([]string, error) {
	keys, err := r.client.HKeys(ctx, r.dataKey()).Result()
	if err != nil {
		return nil, err
	}
	base := strings.TrimSuffix(dir, "/")
	if base == "" {
		return keys, nil
	}
	var below []string
	for _, k := range keys {
		if rel, ok := strings.CutPrefix(k, base+"/"); ok {
			below = append(below, rel)
		}
	}
	return below, nil
}
