package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// expiryGrace delays the safety-net key expiry past ExpiresAt, so the evictor
// normally deletes the hook (and updates indexes and stats) before Redis does.
const expiryGrace = time.Hour

// maxIDAttempts bounds retries on an ID collision, which random IDs make
// theoretical; the check guards against a weak generator.
const maxIDAttempts = 3

var errHookCollision = errors.New("hook id collision")

// Has reports whether a hook exists. A storage error reads as "no", so the
// capture is dropped rather than blocked.
func (m *RedisManager) Has(id string) bool {
	return m.FirstExisting(id) == 0
}

// FirstExisting returns the index of the first id naming a hook, or -1, in
// at most one round-trip: a cached id answers at once, and only the ids
// before it are checked, together.
func (m *RedisManager) FirstExisting(ids ...string) int {
	cached := slices.IndexFunc(ids, m.known.has)
	unknown := ids
	if cached >= 0 {
		unknown = ids[:cached]
	}
	if len(unknown) == 0 {
		return cached
	}
	ctx := context.Background()
	pipe := m.client.Pipeline()
	exists := make([]*redis.IntCmd, len(unknown))
	for i, id := range unknown {
		exists[i] = pipe.Exists(ctx, m.hookKey(id))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		m.logFailure("has", err, "hook_id", unknown[0])
		return cached
	}
	for i, cmd := range exists {
		if cmd.Val() == 1 {
			m.known.add(unknown[i])
			return i
		}
	}
	return cached
}

// CreateHook stores a long-lived hook when its TTL exceeds the ephemeral
// threshold, like the composite manager, without the long-lived cap. On a
// storage error it logs and returns a hook that will never capture;
// registration goes through CreateHookBatch, which reports the error.
func (m *RedisManager) CreateHook(domain string, opts CreateOptions) *Hook {
	hooks, err := m.createHooks(domain, []CreateOptions{opts}, m.kindByTTL, 0)
	if err != nil {
		m.logFailure("create hook", err)
		return newHook(m.idGenerator(), domain, opts)
	}
	return hooks[0]
}

// CreateHookBatch creates ephemeral and long-lived hooks together, all or
// nothing, with maxLongLived capping the long-lived ones.
func (m *RedisManager) CreateHookBatch(domain string, opts []CreateOptions, maxLongLived int) ([]*Hook, error) {
	return m.createHooks(domain, opts, m.kindByTTL, maxLongLived)
}

// CreateLongLivedHook stores a long-lived hook, enforcing maxHooks atomically.
func (m *RedisManager) CreateLongLivedHook(domain string, opts CreateOptions, maxHooks int) (*Hook, error) {
	hooks, err := m.CreateLongLivedHooks(domain, []CreateOptions{opts}, maxHooks)
	if err != nil {
		return nil, err
	}
	return hooks[0], nil
}

// CreateLongLivedHooks stores every hook of the batch, or none of them.
func (m *RedisManager) CreateLongLivedHooks(domain string, opts []CreateOptions, maxHooks int) ([]*Hook, error) {
	if !m.longLivedEnabled {
		return nil, errors.New("long-lived hooks are disabled")
	}
	return m.createHooks(domain, opts, func(CreateOptions) string { return "ll" }, maxHooks)
}

// kindByTTL routes a hook like the composite manager does.
func (m *RedisManager) kindByTTL(o CreateOptions) string {
	if m.longLivedEnabled && o.TTL > m.ephemeralThreshold {
		return "ll"
	}
	return "eph"
}

func (m *RedisManager) createHooks(domain string, opts []CreateOptions, kind func(CreateOptions) string, maxLongLived int) ([]*Hook, error) {
	for attempt := 1; ; attempt++ {
		hooks, args, err := m.hookArgs(domain, opts, kind, maxLongLived)
		if err != nil {
			return nil, err
		}
		err = m.run(context.Background(), createHooksScript, args...).Err()
		switch {
		case err == nil:
			for _, hook := range hooks {
				m.known.add(hook.ID)
			}
			return hooks, nil
		case isScriptError(err, "HOOKD_LIMIT"):
			return nil, ErrHookLimitReached
		case isScriptError(err, "HOOKD_COLLISION") && attempt < maxIDAttempts:
			continue
		case isScriptError(err, "HOOKD_COLLISION"):
			return nil, errHookCollision
		default:
			return nil, fmt.Errorf("persist hooks: %w", err)
		}
	}
}

// hookArgs builds the hooks and the create_hooks.lua arguments for them.
func (m *RedisManager) hookArgs(domain string, opts []CreateOptions, kind func(CreateOptions) string, maxLongLived int) ([]*Hook, []any, error) {
	hooks := make([]*Hook, 0, len(opts))
	args := []any{maxLongLived, len(opts)}
	for _, o := range opts {
		hook := newHook(m.idGenerator(), domain, o)
		var meta string
		if hook.Metadata != nil {
			b, err := json.Marshal(hook.Metadata)
			if err != nil {
				return nil, nil, fmt.Errorf("encode metadata: %w", err)
			}
			meta = string(b)
		}
		expiresMicros, expireAtMillis := "0", int64(0)
		if !hook.ExpiresAt.IsZero() {
			expiresMicros = micros(hook.ExpiresAt)
			expireAtMillis = hook.ExpiresAt.Add(expiryGrace).UnixMilli()
		}
		args = append(args,
			kind(o), hook.ID, hook.CreatedAt.UnixNano(), expiryNanos(hook.ExpiresAt),
			micros(hook.CreatedAt), expiresMicros, expireAtMillis,
			hook.DNS, hook.HTTP, hook.HTTPS, hook.SMTP, meta,
		)
		hooks = append(hooks, hook)
	}
	return hooks, args, nil
}

// isScriptError matches an error raised by redis.error_reply in a script.
func isScriptError(err error, code string) bool {
	return err != nil && strings.Contains(err.Error(), code)
}

// GetHook retrieves a hook by ID.
func (m *RedisManager) GetHook(id string) (*Hook, bool) {
	fields, err := m.client.HGetAll(context.Background(), m.hookKey(id)).Result()
	if err != nil {
		m.logFailure("get hook", err, "hook_id", id)
		return nil, false
	}
	if len(fields) == 0 {
		return nil, false
	}
	return decodeHook(id, fields), true
}

// decodeHook rebuilds a hook from its hash.
func decodeHook(id string, f map[string]string) *Hook {
	hook := &Hook{ID: id, DNS: f["dns"], HTTP: f["http"], HTTPS: f["https"], SMTP: f["smtp"]}
	created, _ := strconv.ParseInt(f["created_at"], 10, 64)
	expires, _ := strconv.ParseInt(f["expires_at"], 10, 64)
	hook.CreatedAt = time.Unix(0, created).UTC()
	if expires != 0 {
		hook.ExpiresAt = time.Unix(0, expires).UTC()
	}
	if meta := f["metadata"]; meta != "" {
		_ = json.Unmarshal([]byte(meta), &hook.Metadata)
	}
	return hook
}

// LongLivedHooks returns every long-lived hook, oldest first.
func (m *RedisManager) LongLivedHooks() ([]*Hook, error) {
	ctx := context.Background()
	ids, err := m.client.ZRange(ctx, m.key("ll"), 0, -1).Result()
	if err != nil {
		return nil, err
	}
	hooks := make([]*Hook, 0, len(ids))
	if len(ids) == 0 {
		return hooks, nil
	}
	pipe := m.client.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, m.hookKey(id))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	for i, cmd := range cmds {
		// Skips a hook whose safety-net expiry fired before the evictor ran.
		if fields := cmd.Val(); len(fields) > 0 {
			hooks = append(hooks, decodeHook(ids[i], fields))
		}
	}
	return hooks, nil
}

// LongLivedCount returns the number of long-lived hooks currently stored.
func (m *RedisManager) LongLivedCount() int {
	n, err := m.client.ZCard(context.Background(), m.key("ll")).Result()
	if err != nil {
		m.logFailure("count long-lived hooks", err)
		return 0
	}
	return int(n)
}
