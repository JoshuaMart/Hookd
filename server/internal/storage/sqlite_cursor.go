package storage

import (
	"database/sql"
)

// ReadInteractions returns the interactions past the cursor without deleting them.
func (m *SQLiteManager) ReadInteractions(hookID string, after int64) (CursorRead, error) {
	return m.ReadOwned(AnyOwner, hookID, after)
}

// ReadOwned is ReadInteractions restricted to the hooks of owner.
func (m *SQLiteManager) ReadOwned(owner, hookID string, after int64) (CursorRead, error) {
	if !m.owns(owner, hookID) {
		return CursorRead{}, ErrHookNotFound
	}

	tx, err := m.readDB.Begin()
	if err != nil {
		return CursorRead{}, err
	}
	defer tx.Rollback()

	var read CursorRead
	err = tx.QueryRow(`SELECT dropped_through FROM hooks WHERE id = ?`, hookID).Scan(&read.DroppedThrough)
	if err == sql.ErrNoRows {
		return CursorRead{}, ErrHookNotFound
	}
	if err != nil {
		return CursorRead{}, err
	}
	if read.Interactions, err = queryInteractions(tx, hookID, after); err != nil {
		return CursorRead{}, err
	}
	return read, nil
}

// AckInteractions deletes the interactions up to and including through.
func (m *SQLiteManager) AckInteractions(hookID string, through int64) (int, error) {
	return m.AckOwned(AnyOwner, hookID, through)
}

// AckOwned is AckInteractions restricted to the hooks of owner.
func (m *SQLiteManager) AckOwned(owner, hookID string, through int64) (int, error) {
	if !m.owns(owner, hookID) {
		return 0, ErrHookNotFound
	}
	res, err := m.db.Exec(`DELETE FROM interactions WHERE hook_id = ? AND seq <= ?`, hookID, through)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
