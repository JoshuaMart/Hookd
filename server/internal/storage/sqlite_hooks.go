package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

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
	hooks, err := m.CreateLongLivedHooks(domain, []CreateOptions{opts}, maxHooks)
	if err != nil {
		return nil, err
	}
	return hooks[0], nil
}

// CreateLongLivedHooks persists a batch in one transaction, under the same lock
// as the cap check, so either every hook exists afterwards or none does.
func (m *SQLiteManager) CreateLongLivedHooks(domain string, opts []CreateOptions, maxHooks int) ([]*Hook, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if maxHooks > 0 && len(m.known)+len(opts) > maxHooks {
		return nil, ErrHookLimitReached
	}

	tx, err := m.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin hook batch: %w", err)
	}
	defer tx.Rollback()

	hooks := make([]*Hook, 0, len(opts))
	for _, o := range opts {
		hook := newHook(m.idGenerator(), domain, o)

		if err := insertHook(tx, hook); err != nil {
			return nil, err
		}

		hooks = append(hooks, hook)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit hook batch: %w", err)
	}
	for _, hook := range hooks {
		m.known[hook.ID] = struct{}{}
	}
	return hooks, nil
}

// insertHook encodes and inserts one row within the caller's batch transaction.
func insertHook(tx *sql.Tx, hook *Hook) error {
	var meta sql.NullString
	if hook.Metadata != nil {
		b, err := json.Marshal(hook.Metadata)
		if err != nil {
			return fmt.Errorf("encode metadata: %w", err)
		}
		meta = sql.NullString{String: string(b), Valid: true}
	}

	if _, err := tx.Exec(
		`INSERT INTO hooks (id, dns, http, https, smtp, created_at, expires_at, metadata) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		hook.ID, hook.DNS, hook.HTTP, hook.HTTPS, hook.SMTP, hook.CreatedAt.UnixNano(), expiryNanos(hook.ExpiresAt), meta,
	); err != nil {
		return fmt.Errorf("persist long-lived hook: %w", err)
	}
	return nil
}

// LongLivedHooks returns every long-lived hook, oldest first.
func (m *SQLiteManager) LongLivedHooks() ([]*Hook, error) {
	rows, err := m.readDB.Query(
		`SELECT id, dns, http, https, smtp, created_at, expires_at, metadata FROM hooks ORDER BY created_at, id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hooks []*Hook
	for rows.Next() {
		hook, err := scanHook(rows)
		if err != nil {
			return nil, err
		}
		hooks = append(hooks, hook)
	}
	return hooks, rows.Err()
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
	row := m.readDB.QueryRow(
		`SELECT id, dns, http, https, smtp, created_at, expires_at, metadata FROM hooks WHERE id = ?`, id,
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
