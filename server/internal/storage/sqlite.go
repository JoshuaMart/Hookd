package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo, cross-compiles cleanly)
)

// sqliteSchema is applied at startup. Timestamps are stored as Unix nanoseconds
// so they compare and range-scan as plain integers. A zero expires_at means
// "no explicit expiry".
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS hooks (
	id         TEXT PRIMARY KEY,
	dns        TEXT NOT NULL,
	http       TEXT NOT NULL,
	https      TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	metadata   TEXT
);
CREATE INDEX IF NOT EXISTS idx_hooks_expires ON hooks(expires_at);

CREATE TABLE IF NOT EXISTS interactions (
	id         TEXT PRIMARY KEY,
	hook_id    TEXT NOT NULL REFERENCES hooks(id) ON DELETE CASCADE,
	type       TEXT NOT NULL,
	timestamp  INTEGER NOT NULL,
	source_ip  TEXT,
	data       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_interactions_hook ON interactions(hook_id);
CREATE INDEX IF NOT EXISTS idx_interactions_ts ON interactions(timestamp);
`

// SQLiteManager persists long-lived hooks and their interactions to a SQLite
// database so they survive restarts. It keeps an in-memory set of the hook IDs
// it owns so the composite manager can route captures without touching disk.
type SQLiteManager struct {
	db           *sql.DB
	idGenerator  func() string
	maxBodyBytes int
	logger       *slog.Logger

	mu    sync.RWMutex
	known map[string]struct{}
}

// NewSQLiteManager opens (creating if needed) the database at dbPath and loads
// the set of known hook IDs into memory.
func NewSQLiteManager(dbPath string, idGenerator func() string, maxBodyBytes int, logger *slog.Logger) (*SQLiteManager, error) {
	// Ensure the parent directory exists so a fresh host boots with the default
	// db_path (e.g. /var/lib/hookd/longlived.db) instead of failing to open.
	if dir := filepath.Dir(dbPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
	}

	// Per-connection pragmas via the DSN so every pooled connection gets them:
	// WAL, a busy timeout, and cascading deletes.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)",
		dbPath,
	)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}

	// Serialize access to the single database file. SQLite allows only one
	// writer at a time; with an unbounded pool a deferred read→write transaction
	// (e.g. PollInteractions) can hit SQLITE_BUSY_SNAPSHOT, which busy_timeout
	// does not retry. One connection removes writer contention entirely. The hot
	// capture path is in-memory, so long-lived throughput is not a bottleneck.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(sqliteSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	m := &SQLiteManager{
		db:           db,
		idGenerator:  idGenerator,
		maxBodyBytes: maxBodyBytes,
		logger:       logger,
		known:        make(map[string]struct{}),
	}

	if err := m.loadIndex(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("load hook index: %w", err)
	}

	return m, nil
}

// loadIndex populates the in-memory hook-ID set from the database.
func (m *SQLiteManager) loadIndex() error {
	rows, err := m.db.Query(`SELECT id FROM hooks`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		m.known[id] = struct{}{}
	}
	return rows.Err()
}

// Close closes the underlying database.
func (m *SQLiteManager) Close() error {
	return m.db.Close()
}

// Has reports whether the given hook ID is stored here. It is a fast in-memory
// lookup used by the composite manager to route captures.
func (m *SQLiteManager) Has(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.known[id]
	return ok
}

// LongLivedCount returns the number of long-lived hooks currently stored.
func (m *SQLiteManager) LongLivedCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.known)
}

// CreateLongLivedHook atomically enforces the cap and persists a new hook. The
// capacity check, INSERT and index update happen under one lock, so the
// MaxHooks invariant holds under concurrent registration and a persistence
// failure is surfaced instead of yielding a hook that never captures.
func (m *SQLiteManager) CreateLongLivedHook(domain string, opts CreateOptions, maxHooks int) (*Hook, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if maxHooks > 0 && len(m.known) >= maxHooks {
		return nil, ErrHookLimitReached
	}

	hook := newHook(m.idGenerator(), domain, opts)

	var meta sql.NullString
	if opts.Metadata != nil {
		b, err := json.Marshal(opts.Metadata)
		if err != nil {
			return nil, fmt.Errorf("encode metadata: %w", err)
		}
		meta = sql.NullString{String: string(b), Valid: true}
	}

	if _, err := m.db.Exec(
		`INSERT INTO hooks (id, dns, http, https, created_at, expires_at, metadata) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		hook.ID, hook.DNS, hook.HTTP, hook.HTTPS, hook.CreatedAt.UnixNano(), expiryNanos(hook.ExpiresAt), meta,
	); err != nil {
		return nil, fmt.Errorf("persist long-lived hook: %w", err)
	}

	m.known[hook.ID] = struct{}{}
	return hook, nil
}

// CreateHook satisfies the Manager interface. Production registration goes
// through CreateLongLivedHook (which can report failure); this uncapped wrapper
// exists so the composite and tests can create via the generic interface. On a
// persistence error it logs and returns the hook without registering it.
func (m *SQLiteManager) CreateHook(domain string, opts CreateOptions) *Hook {
	hook, err := m.CreateLongLivedHook(domain, opts, 0)
	if err != nil {
		m.logger.Error("failed to persist long-lived hook", "error", err)
		return newHook(m.idGenerator(), domain, opts)
	}
	return hook
}

// GetHook retrieves a hook by ID.
func (m *SQLiteManager) GetHook(id string) (*Hook, bool) {
	if !m.Has(id) {
		return nil, false
	}
	row := m.db.QueryRow(
		`SELECT id, dns, http, https, created_at, expires_at, metadata FROM hooks WHERE id = ?`, id,
	)
	hook, err := scanHook(row)
	if err != nil {
		if err != sql.ErrNoRows {
			m.logger.Error("failed to read long-lived hook", "error", err, "id", id)
		}
		return nil, false
	}
	return hook, true
}

// AddInteraction stores an interaction for a known hook. Interactions for hooks
// this store does not own are ignored (the composite routes them elsewhere).
func (m *SQLiteManager) AddInteraction(hookID string, interaction *Interaction) {
	if !m.Has(hookID) {
		return
	}

	m.truncateBody(interaction)
	data, err := json.Marshal(interaction.Data)
	if err != nil {
		m.logger.Error("failed to encode interaction data", "error", err, "hook_id", hookID)
		return
	}

	if _, err := m.db.Exec(
		`INSERT INTO interactions (id, hook_id, type, timestamp, source_ip, data) VALUES (?, ?, ?, ?, ?, ?)`,
		interaction.ID, hookID, string(interaction.Type), interaction.Timestamp.UnixNano(), interaction.SourceIP, string(data),
	); err != nil {
		// The hook can be cascade-deleted (expiry) between the Has() check above
		// and this insert; the resulting FK failure means the hook is gone, so
		// dropping the interaction is correct, not an error worth alerting on.
		m.logger.Debug("failed to persist interaction", "error", err, "hook_id", hookID)
	}
}

// truncateBody caps the stored HTTP body at maxBodyBytes, flagging the entry so
// clients know it was cut. Long-lived hooks can accumulate for weeks, so full
// multi-megabyte bodies are not kept on disk. The cut is made on a UTF-8 rune
// boundary so the stored body is never left with a mangled trailing rune.
func (m *SQLiteManager) truncateBody(interaction *Interaction) {
	body, ok := interaction.Data["body"].(string)
	if !ok {
		return
	}
	cut, truncated := TruncateBody(body, m.maxBodyBytes)
	if !truncated {
		return
	}
	interaction.Data["body"] = cut
	interaction.Data["truncated"] = true
}

// PollInteractions atomically returns and deletes all interactions for a hook.
func (m *SQLiteManager) PollInteractions(hookID string) []*Interaction {
	if !m.Has(hookID) {
		return []*Interaction{}
	}

	tx, err := m.db.Begin()
	if err != nil {
		m.logger.Error("failed to begin poll transaction", "error", err, "hook_id", hookID)
		return []*Interaction{}
	}
	defer tx.Rollback()

	interactions, err := queryInteractions(tx, hookID)
	if err != nil {
		m.logger.Error("failed to read interactions", "error", err, "hook_id", hookID)
		return []*Interaction{}
	}

	if len(interactions) > 0 {
		if _, err := tx.Exec(`DELETE FROM interactions WHERE hook_id = ?`, hookID); err != nil {
			m.logger.Error("failed to clear interactions", "error", err, "hook_id", hookID)
			return []*Interaction{}
		}
	}

	if err := tx.Commit(); err != nil {
		m.logger.Error("failed to commit poll", "error", err, "hook_id", hookID)
		return []*Interaction{}
	}
	return interactions
}

// PollInteractionsBatch polls several hooks. Unknown hooks yield a not-found
// error, matching the in-memory manager's contract.
func (m *SQLiteManager) PollInteractionsBatch(hookIDs []string) map[string]*PollResult {
	results := make(map[string]*PollResult, len(hookIDs))
	for _, id := range hookIDs {
		if !m.Has(id) {
			results[id] = &PollResult{Error: "Hook not found"}
			continue
		}
		results[id] = &PollResult{Interactions: m.PollInteractions(id)}
	}
	return results
}

// Stats reports hook and interaction counts. Memory statistics are left zero;
// the composite manager fills those in from the in-memory store.
func (m *SQLiteManager) Stats() Stats {
	stats := Stats{}

	m.mu.RLock()
	stats.HooksActive = len(m.known)
	m.mu.RUnlock()

	rows, err := m.db.Query(`SELECT type, COUNT(*) FROM interactions GROUP BY type`)
	if err != nil {
		m.logger.Error("failed to read interaction stats", "error", err)
		return stats
	}
	defer rows.Close()

	for rows.Next() {
		var typ string
		var count int
		if err := rows.Scan(&typ, &count); err != nil {
			m.logger.Error("failed to scan interaction stats", "error", err)
			return stats
		}
		stats.InteractionsTotal += count
		switch InteractionType(typ) {
		case InteractionTypeDNS:
			stats.InteractionsDNS += count
		case InteractionTypeHTTP:
			stats.InteractionsHTTP += count
		}
	}
	return stats
}

// EvictInteractionsBefore is a no-op: long-lived interactions are retained until
// the hook is polled or expires, not aged out by the interaction TTL.
func (m *SQLiteManager) EvictInteractionsBefore(_ time.Time) int { return 0 }

// EvictExpiredHooks removes hooks whose expiry has passed (cascading to their
// interactions) and returns how many were removed.
func (m *SQLiteManager) EvictExpiredHooks(now time.Time) int {
	rows, err := m.db.Query(
		`SELECT id FROM hooks WHERE expires_at != 0 AND expires_at < ?`, now.UnixNano(),
	)
	if err != nil {
		m.logger.Error("failed to find expired hooks", "error", err)
		return 0
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			m.logger.Error("failed to scan expired hook", "error", err)
			return 0
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if len(ids) == 0 {
		return 0
	}

	// Drop the ids from the index *before* deleting the rows. A concurrent
	// capture then sees Has()==false and is routed away cleanly, rather than
	// passing Has() and hitting an FK failure when the row disappears mid-insert.
	m.mu.Lock()
	for _, id := range ids {
		delete(m.known, id)
	}
	m.mu.Unlock()

	if _, err := m.db.Exec(
		`DELETE FROM hooks WHERE expires_at != 0 AND expires_at < ?`, now.UnixNano(),
	); err != nil {
		m.logger.Error("failed to delete expired hooks", "error", err)
		// The index no longer lists these hooks; the rows are cleaned up on a
		// later tick. Report them as evicted to keep the count consistent.
	}
	return len(ids)
}

// EnforcePerHookLimit trims each hook to at most max interactions, dropping the
// oldest first, and returns the number removed.
func (m *SQLiteManager) EnforcePerHookLimit(max int) int {
	rows, err := m.db.Query(
		`SELECT hook_id, COUNT(*) FROM interactions GROUP BY hook_id HAVING COUNT(*) > ?`, max,
	)
	if err != nil {
		m.logger.Error("failed to find over-limit hooks", "error", err)
		return 0
	}
	type overflow struct {
		hookID string
		count  int
	}
	var overflows []overflow
	for rows.Next() {
		var o overflow
		if err := rows.Scan(&o.hookID, &o.count); err != nil {
			_ = rows.Close()
			m.logger.Error("failed to scan over-limit hook", "error", err)
			return 0
		}
		overflows = append(overflows, o)
	}
	_ = rows.Close()

	total := 0
	for _, o := range overflows {
		// Delete everything except the newest `max` rows for this hook.
		res, err := m.db.Exec(
			`DELETE FROM interactions WHERE hook_id = ? AND id NOT IN (
				SELECT id FROM interactions WHERE hook_id = ? ORDER BY timestamp DESC, id DESC LIMIT ?
			)`,
			o.hookID, o.hookID, max,
		)
		if err != nil {
			m.logger.Error("failed to enforce per-hook limit", "error", err, "hook_id", o.hookID)
			continue
		}
		if n, err := res.RowsAffected(); err == nil {
			total += int(n)
		}
	}
	return total
}

// EvictByMemoryPressure is a no-op: long-lived data lives on disk, not the heap.
func (m *SQLiteManager) EvictByMemoryPressure(_ int) MemoryEvictionResult {
	return MemoryEvictionResult{}
}

// LongLivedActivity returns the long-lived hooks that currently have pending
// interactions, with their pending count and most recent interaction time.
func (m *SQLiteManager) LongLivedActivity() []HookActivity {
	rows, err := m.db.Query(`
		SELECT h.id, h.dns, h.http, h.https, h.created_at, h.expires_at, h.metadata,
		       COUNT(i.id), COALESCE(MAX(i.timestamp), 0)
		FROM hooks h
		JOIN interactions i ON i.hook_id = h.id
		GROUP BY h.id
		HAVING COUNT(i.id) > 0
		ORDER BY MAX(i.timestamp) DESC`)
	if err != nil {
		m.logger.Error("failed to read long-lived activity", "error", err)
		return nil
	}
	defer rows.Close()

	var activity []HookActivity
	for rows.Next() {
		var (
			hook             Hook
			createdNanos     int64
			expiresNanos     int64
			meta             sql.NullString
			pendingCount     int
			lastInteractNano int64
		)
		if err := rows.Scan(
			&hook.ID, &hook.DNS, &hook.HTTP, &hook.HTTPS, &createdNanos, &expiresNanos, &meta,
			&pendingCount, &lastInteractNano,
		); err != nil {
			m.logger.Error("failed to scan long-lived activity", "error", err)
			return activity
		}
		assignHookTimes(&hook, createdNanos, expiresNanos, meta)

		activity = append(activity, HookActivity{
			Hook:              &hook,
			PendingCount:      pendingCount,
			LastInteractionAt: time.Unix(0, lastInteractNano).UTC(),
		})
	}
	return activity
}

// scanner is the shared behaviour of *sql.Row and *sql.Rows needed to read a hook.
type scanner interface {
	Scan(dest ...any) error
}

// scanHook reads a full hook row.
func scanHook(s scanner) (*Hook, error) {
	var (
		hook         Hook
		createdNanos int64
		expiresNanos int64
		meta         sql.NullString
	)
	if err := s.Scan(&hook.ID, &hook.DNS, &hook.HTTP, &hook.HTTPS, &createdNanos, &expiresNanos, &meta); err != nil {
		return nil, err
	}
	assignHookTimes(&hook, createdNanos, expiresNanos, meta)
	return &hook, nil
}

// assignHookTimes decodes the stored nanos/metadata columns onto a hook. The
// zero expires_at sentinel maps back to a zero ExpiresAt.
func assignHookTimes(hook *Hook, createdNanos, expiresNanos int64, meta sql.NullString) {
	hook.CreatedAt = time.Unix(0, createdNanos).UTC()
	if expiresNanos != 0 {
		hook.ExpiresAt = time.Unix(0, expiresNanos).UTC()
	}
	hook.Metadata = decodeMetadata(meta)
}

// queryInteractions reads all interactions for a hook in arrival order.
func queryInteractions(q interface {
	Query(query string, args ...any) (*sql.Rows, error)
}, hookID string) ([]*Interaction, error) {
	rows, err := q.Query(
		`SELECT id, type, timestamp, source_ip, data FROM interactions WHERE hook_id = ? ORDER BY timestamp, id`, hookID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	interactions := make([]*Interaction, 0)
	for rows.Next() {
		var (
			it       Interaction
			typ      string
			tsNanos  int64
			sourceIP sql.NullString
			dataJSON string
		)
		if err := rows.Scan(&it.ID, &typ, &tsNanos, &sourceIP, &dataJSON); err != nil {
			return nil, err
		}
		it.Type = InteractionType(typ)
		it.Timestamp = time.Unix(0, tsNanos).UTC()
		it.SourceIP = sourceIP.String
		if err := json.Unmarshal([]byte(dataJSON), &it.Data); err != nil {
			return nil, err
		}
		interactions = append(interactions, &it)
	}
	return interactions, rows.Err()
}

// decodeMetadata parses a nullable metadata JSON column, returning nil when
// absent or unparseable.
func decodeMetadata(meta sql.NullString) map[string]any {
	if !meta.Valid || meta.String == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(meta.String), &m); err != nil {
		return nil
	}
	return m
}

// expiryNanos converts an expiry time to its stored representation (0 = none).
func expiryNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}
