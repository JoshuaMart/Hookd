package storage

import (
	"database/sql"
	"time"
)

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
		n, err := m.trimHook(o.hookID, max)
		if err != nil {
			m.logger.Error("failed to enforce per-hook limit", "error", err, "hook_id", o.hookID)
			continue
		}
		total += n
	}
	return total
}

// trimHook keeps the newest max interactions of a hook and records the highest
// dropped seq so cursor readers can detect the loss.
func (m *SQLiteManager) trimHook(hookID string, max int) (int, error) {
	tx, err := m.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	n, err := trimHookInteractions(tx, hookID, max)
	if err != nil || n == 0 {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// trimHookInteractions deletes overflow and records cursor loss in the same
// transaction. The caller rolls both changes back if either operation fails.
func trimHookInteractions(tx *sql.Tx, hookID string, max int) (int, error) {
	var cutoff int64
	err := tx.QueryRow(
		`SELECT seq FROM interactions WHERE hook_id = ? ORDER BY seq DESC LIMIT 1 OFFSET ?`, hookID, max,
	).Scan(&cutoff)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	res, err := tx.Exec(`DELETE FROM interactions WHERE hook_id = ? AND seq <= ?`, hookID, cutoff)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(
		`UPDATE hooks SET dropped_through = MAX(dropped_through, ?) WHERE id = ?`, cutoff, hookID,
	); err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// EvictByMemoryPressure is a no-op: long-lived data lives on disk, not the heap.
func (m *SQLiteManager) EvictByMemoryPressure(_ int) MemoryEvictionResult {
	return MemoryEvictionResult{}
}
