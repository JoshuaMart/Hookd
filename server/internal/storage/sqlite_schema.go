package storage

import (
	"database/sql"
	"fmt"
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
	smtp       TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	metadata   TEXT,
	last_seq        INTEGER NOT NULL DEFAULT 0,
	dropped_through INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_hooks_expires ON hooks(expires_at);

CREATE TABLE IF NOT EXISTS interactions (
	id         TEXT PRIMARY KEY,
	hook_id    TEXT NOT NULL REFERENCES hooks(id) ON DELETE CASCADE,
	type       TEXT NOT NULL,
	timestamp  INTEGER NOT NULL,
	source_ip  TEXT,
	data       TEXT NOT NULL,
	seq        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_interactions_ts ON interactions(timestamp);
`

// initializeSQLiteSchema supports both fresh and existing databases. CREATE
// TABLE IF NOT EXISTS alone would leave older installations missing columns.
func initializeSQLiteSchema(db *sql.DB) error {
	if _, err := db.Exec(sqliteSchema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(db); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	return nil
}

// migrate brings a database up to the current schema. Every step is idempotent.
func migrate(db *sql.DB) error {
	// Older rows keep '' for smtp: the store knows neither the domain nor
	// whether SMTP is enabled, so backfilling could advertise a black hole.
	for _, col := range []struct{ table, name, ddl string }{
		{"hooks", "smtp", "TEXT NOT NULL DEFAULT ''"},
		{"hooks", "last_seq", "INTEGER NOT NULL DEFAULT 0"},
		{"hooks", "dropped_through", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if _, err := addColumn(db, col.table, col.name, col.ddl); err != nil {
			return err
		}
	}

	if err := migrateSeq(db); err != nil {
		return err
	}

	// (hook_id, seq) covers every lookup the hook_id index served.
	if _, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_interactions_hook_seq ON interactions(hook_id, seq);
		DROP INDEX IF EXISTS idx_interactions_hook;`); err != nil {
		return fmt.Errorf("index interactions.seq: %w", err)
	}
	return nil
}

// migrateSeq adds interactions.seq and numbers pending rows in arrival order,
// in one transaction so a crash cannot leave the column without its backfill.
func migrateSeq(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	added, err := addColumn(tx, "interactions", "seq", "INTEGER NOT NULL DEFAULT 0")
	if err != nil || !added {
		return err
	}
	if _, err := tx.Exec(`
		UPDATE interactions SET seq = n.rn FROM (
			SELECT id, ROW_NUMBER() OVER (PARTITION BY hook_id ORDER BY timestamp, id) AS rn FROM interactions
		) AS n WHERE interactions.id = n.id;
		UPDATE hooks SET last_seq = (
			SELECT COALESCE(MAX(seq), 0) FROM interactions WHERE hook_id = hooks.id);`); err != nil {
		return fmt.Errorf("backfill interactions.seq: %w", err)
	}
	return tx.Commit()
}

// execQuerier is the subset of *sql.DB and *sql.Tx the migration helpers use.
type execQuerier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
}

// addColumn adds a column when missing and reports whether it did.
func addColumn(db execQuerier, table, column, ddl string) (bool, error) {
	has, err := hasColumn(db, table, column)
	if err != nil || has {
		return false, err
	}
	if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, ddl)); err != nil {
		return false, fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return true, nil
}

// hasColumn reports whether a table already has the given column.
func hasColumn(db execQuerier, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid        int
			name       string
			typ        string
			notNull    int
			dfltValue  sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dfltValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
