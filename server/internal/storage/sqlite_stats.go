package storage

import (
	"database/sql"
	"time"
)

// Stats reports hook and interaction counts. Memory statistics are left zero;
// the composite manager fills those in from the in-memory store.
func (m *SQLiteManager) Stats() Stats {
	stats := Stats{}

	m.mu.RLock()
	stats.HooksActive = len(m.known)
	m.mu.RUnlock()

	rows, err := m.readDB.Query(`SELECT type, COUNT(*) FROM interactions GROUP BY type`)
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
		case InteractionTypeSMTP:
			stats.InteractionsSMTP += count
		}
	}
	return stats
}

// LongLivedActivity returns the long-lived hooks that currently have pending
// interactions, with their pending count and most recent interaction time.
func (m *SQLiteManager) LongLivedActivity() []HookActivity {
	rows, err := m.readDB.Query(`
		SELECT h.id, h.dns, h.http, h.https, h.smtp, h.created_at, h.expires_at, h.metadata,
		       COUNT(i.id), COALESCE(MAX(i.timestamp), 0), COALESCE(MAX(i.seq), 0)
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
			lastSeq          int64
		)
		if err := rows.Scan(
			&hook.ID, &hook.DNS, &hook.HTTP, &hook.HTTPS, &hook.SMTP, &createdNanos, &expiresNanos, &meta,
			&pendingCount, &lastInteractNano, &lastSeq,
		); err != nil {
			m.logger.Error("failed to scan long-lived activity", "error", err)
			return activity
		}
		assignHookTimes(&hook, createdNanos, expiresNanos, meta)

		activity = append(activity, HookActivity{
			Hook:              &hook,
			PendingCount:      pendingCount,
			LastInteractionAt: time.Unix(0, lastInteractNano).UTC(),
			LastSeq:           lastSeq,
		})
	}
	return activity
}
