package storage

import (
	"database/sql"
	"encoding/json"
	"time"
)

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
	if err := s.Scan(&hook.ID, &hook.DNS, &hook.HTTP, &hook.HTTPS, &hook.SMTP, &createdNanos, &expiresNanos, &meta); err != nil {
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

// queryInteractions reads a hook's interactions past after, in seq order.
func queryInteractions(q interface {
	Query(query string, args ...any) (*sql.Rows, error)
}, hookID string, after int64) ([]*Interaction, error) {
	rows, err := q.Query(
		`SELECT id, seq, type, timestamp, source_ip, data FROM interactions WHERE hook_id = ? AND seq > ? ORDER BY seq, timestamp, id`,
		hookID, after,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	interactions := make([]*Interaction, 0)
	for rows.Next() {
		it, err := scanInteraction(rows)
		if err != nil {
			return nil, err
		}

		interactions = append(interactions, it)
	}
	return interactions, rows.Err()
}

// scanInteraction decodes a row before it can be returned or drained.
func scanInteraction(s scanner) (*Interaction, error) {
	var (
		it       Interaction
		typ      string
		tsNanos  int64
		sourceIP sql.NullString
		dataJSON string
	)
	if err := s.Scan(&it.ID, &it.Seq, &typ, &tsNanos, &sourceIP, &dataJSON); err != nil {
		return nil, err
	}
	it.Type = InteractionType(typ)
	it.Timestamp = time.Unix(0, tsNanos).UTC()
	it.SourceIP = sourceIP.String
	if err := json.Unmarshal([]byte(dataJSON), &it.Data); err != nil {
		return nil, err
	}
	return &it, nil
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
