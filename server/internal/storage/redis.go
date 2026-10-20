package storage

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisManager keeps every hook and interaction in a Redis or Valkey server,
// so several Hookd instances behind a load balancer share one view. Each
// operation that reads then writes is a Lua script, which Redis runs
// atomically; the key layout is described in scripts/prelude.lua.
//
// The scripts touch per-hook and global keys together, so the server must
// not run in cluster mode.
type RedisManager struct {
	client redis.UniversalClient
	prefix string
	owner  string // identifies this instance for the eviction lease

	idGenerator        func() string
	ephemeralThreshold time.Duration
	longLivedEnabled   bool
	maxPerHook         int
	maxBodyBytes       int
	maxPerOwner        int
	logger             *slog.Logger
	known              *hasCache

	// The background writer behind RecordAsync.
	queue  chan pendingCapture
	stop   chan struct{}
	done   chan struct{}
	closed atomic.Bool

	lastFailureLog sync.Map // op -> *atomic.Int64, unix nanos
}

// RedisOptions configures a RedisManager.
type RedisOptions struct {
	// KeyPrefix namespaces every key; it must not be empty.
	KeyPrefix   string
	IDGenerator func() string
	// EphemeralThreshold is the ephemeral hook TTL: CreateHook stores hooks
	// with a longer TTL as long-lived, like the composite manager.
	EphemeralThreshold time.Duration
	LongLivedEnabled   bool
	// MaxPerHook trims each hook on insert, like MemoryManager.SetMaxPerHook.
	// Zero leaves it to the periodic EnforcePerHookLimit pass.
	MaxPerHook int
	// MaxBodyBytes truncates bodies stored for long-lived hooks, like SQLite.
	MaxBodyBytes int
	// MaxLongLivedPerOwner caps the long-lived hooks of each owner; zero
	// disables it.
	MaxLongLivedPerOwner int
	// HasCacheSize bounds the hook IDs remembered as existing for
	// HasCacheTTL, which spares most captures a round-trip. A hook deleted on
	// another instance can then still read as existing until the TTL ends.
	// Zero disables the cache.
	HasCacheSize int
	HasCacheTTL  time.Duration
	Logger       *slog.Logger
}

// Compile-time checks that RedisManager satisfies the storage interfaces.
var (
	_ Store          = (*RedisManager)(nil)
	_ EvictionLeaser = (*RedisManager)(nil)
	_ AsyncRecorder  = (*RedisManager)(nil)
	_ BatchCreator   = (*RedisManager)(nil)
)

// minRedisMajor is the oldest server with every command the scripts use
// (PEXPIRETIME, ZADD GT, XTRIM MINID).
const minRedisMajor = 7

// NewRedisManager checks the server is reachable and recent enough. The
// client stays owned by the manager and is closed by Close; it needs
// ContextTimeoutEnabled, or the deadlines bounding background writes are
// ignored.
func NewRedisManager(ctx context.Context, client redis.UniversalClient, opts RedisOptions) (*RedisManager, error) {
	if opts.KeyPrefix == "" {
		return nil, errors.New("redis key prefix must not be empty")
	}
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("connect to redis: %w", err)
	}
	if err := checkRedisVersion(ctx, client); err != nil {
		return nil, err
	}
	// Loaded up front, so pipelined calls do not start with NOSCRIPT.
	for _, script := range allScripts {
		if err := script.Load(ctx, client).Err(); err != nil {
			return nil, fmt.Errorf("load redis scripts: %w", err)
		}
	}

	owner := make([]byte, 8)
	if _, err := rand.Read(owner); err != nil {
		return nil, fmt.Errorf("generate lease owner: %w", err)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	m := &RedisManager{
		client:             client,
		prefix:             opts.KeyPrefix,
		owner:              hex.EncodeToString(owner),
		idGenerator:        opts.IDGenerator,
		ephemeralThreshold: opts.EphemeralThreshold,
		longLivedEnabled:   opts.LongLivedEnabled,
		maxPerHook:         opts.MaxPerHook,
		maxBodyBytes:       opts.MaxBodyBytes,
		maxPerOwner:        opts.MaxLongLivedPerOwner,
		logger:             logger,
		known:              newHasCache(opts.HasCacheSize, opts.HasCacheTTL),
		queue:              make(chan pendingCapture, asyncQueueSize),
		stop:               make(chan struct{}),
		done:               make(chan struct{}),
	}
	go m.writeLoop()
	return m, nil
}

// checkRedisVersion reads redis_version, which Valkey also reports.
func checkRedisVersion(ctx context.Context, client redis.UniversalClient) error {
	info, err := client.Info(ctx, "server").Result()
	if err != nil {
		return fmt.Errorf("read redis version: %w", err)
	}
	for _, line := range strings.Split(info, "\n") {
		version, ok := strings.CutPrefix(strings.TrimSpace(line), "redis_version:")
		if !ok {
			continue
		}
		major, err := strconv.Atoi(strings.SplitN(version, ".", 2)[0])
		if err != nil {
			return fmt.Errorf("parse redis version %q: %w", version, err)
		}
		if major < minRedisMajor {
			return fmt.Errorf("redis %s is too old: %d.0 or later is required", version, minRedisMajor)
		}
		return nil
	}
	return errors.New("redis did not report its version")
}

// Close writes the queued captures, then releases the connection pool.
func (m *RedisManager) Close() error {
	if !m.closed.Swap(true) {
		close(m.stop)
		<-m.done
	}
	return m.client.Close()
}

func (m *RedisManager) hookKey(id string) string   { return m.prefix + "h:" + id }
func (m *RedisManager) streamKey(id string) string { return m.prefix + "i:" + id }
func (m *RedisManager) key(name string) string     { return m.prefix + name }
func (m *RedisManager) ownerKey(o string) string   { return m.prefix + "o:" + o }

// run executes a script with the prefix as ARGV[1], as every script expects.
func (m *RedisManager) run(ctx context.Context, s *redis.Script, args ...any) *redis.Cmd {
	return s.Run(ctx, m.client, nil, append([]any{m.prefix}, args...)...)
}

// logFailure reports a storage error at most once a second per operation:
// when the server is unreachable every DNS query fails, and one line per
// query would flood the log, but a burst of one failure must not hide
// another kind.
func (m *RedisManager) logFailure(op string, err error, attrs ...any) {
	v, _ := m.lastFailureLog.LoadOrStore(op, new(atomic.Int64))
	last := v.(*atomic.Int64)
	now := time.Now().UnixNano()
	prev := last.Load()
	if now-prev < int64(time.Second) || !last.CompareAndSwap(prev, now) {
		return
	}
	m.logger.Error("redis storage "+op+" failed", append([]any{"error", err}, attrs...)...)
}

// micros and streamID convert to the units the scripts store. Scores are µs
// so they stay exact in a Redis double.
func micros(t time.Time) string { return strconv.FormatInt(t.UnixMicro(), 10) }

func streamID(seq int64) string { return strconv.FormatInt(seq, 10) + "-0" }

//go:embed scripts/*.lua
var scriptFiles embed.FS

// newScript prepends the shared helpers to a script.
func newScript(name string) *redis.Script {
	prelude, err := scriptFiles.ReadFile("scripts/prelude.lua")
	if err != nil {
		panic(err)
	}
	body, err := scriptFiles.ReadFile("scripts/" + name + ".lua")
	if err != nil {
		panic(err)
	}
	return redis.NewScript(string(prelude) + "\n" + string(body))
}

var (
	createHooksScript    = newScript("create_hooks")
	getHookScript        = newScript("get_hook")
	addInteractionScript = newScript("add_interaction")
	readScript           = newScript("read")
	ackScript            = newScript("ack")
	pollScript           = newScript("poll")
	evictExpiredScript   = newScript("evict_expired")
	evictBeforeScript    = newScript("evict_before")
	trimScript           = newScript("trim")
	leaseScript          = newScript("lease")

	allScripts = []*redis.Script{
		createHooksScript, getHookScript, addInteractionScript, readScript, ackScript, pollScript,
		evictExpiredScript, evictBeforeScript, trimScript, leaseScript,
	}
)
