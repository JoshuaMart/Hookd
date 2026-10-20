package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo, cross-compiles cleanly)
)

// SQLiteManager persists long-lived hooks and their interactions to a SQLite
// database so they survive restarts. It keeps an in-memory set of the hook IDs
// it owns so the composite manager can route captures without touching disk.
type SQLiteManager struct {
	db           *sql.DB
	readDB       *sql.DB
	idGenerator  func() string
	maxBodyBytes int
	logger       *slog.Logger

	mu          sync.RWMutex
	known       map[string]string // id -> owner
	perOwner    map[string]int
	maxPerOwner int
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

	db, err := openSQLiteWriter(dsn)
	if err != nil {
		return nil, err
	}
	initialized := false
	defer func() {
		if !initialized {
			_ = db.Close()
		}
	}()

	m := &SQLiteManager{
		db:           db,
		idGenerator:  idGenerator,
		maxBodyBytes: maxBodyBytes,
		logger:       logger,
		known:        make(map[string]string),
		perOwner:     make(map[string]int),
	}

	if err := m.loadIndex(); err != nil {
		return nil, fmt.Errorf("load hook index: %w", err)
	}

	readers, err := openSQLiteReaders(dsn)
	if err != nil {
		return nil, err
	}
	m.readDB = readers
	initialized = true
	return m, nil
}

// openSQLiteWriter serializes read-modify-write transactions on one connection.
// An unbounded pool can produce SQLITE_BUSY_SNAPSHOT when a deferred read
// transaction upgrades to a write; busy_timeout does not retry that failure.
func openSQLiteWriter(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := initializeSQLiteSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// openSQLiteReaders uses WAL snapshots without occupying the writer connection.
func openSQLiteReaders(dsn string) (*sql.DB, error) {
	readers, err := sql.Open("sqlite", dsn+"&_pragma=query_only(ON)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite readers: %w", err)
	}
	readers.SetMaxOpenConns(4)
	readers.SetMaxIdleConns(4)
	if err := readers.Ping(); err != nil {
		_ = readers.Close()
		return nil, fmt.Errorf("initialize sqlite readers: %w", err)
	}
	return readers, nil
}

// loadIndex populates the in-memory hook index from the database.
func (m *SQLiteManager) loadIndex() error {
	rows, err := m.db.Query(`SELECT id, owner FROM hooks`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var id, owner string
		if err := rows.Scan(&id, &owner); err != nil {
			return err
		}
		m.remember(id, owner)
	}
	return rows.Err()
}

// SetMaxPerOwner caps the long-lived hooks of each owner; zero disables it.
func (m *SQLiteManager) SetMaxPerOwner(max int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxPerOwner = max
}

// remember and forget keep the index and per-owner counts in step. Callers
// must hold m.mu.
func (m *SQLiteManager) remember(id, owner string) {
	m.known[id] = owner
	if owner != "" {
		m.perOwner[owner]++
	}
}

func (m *SQLiteManager) forget(id string) {
	owner, ok := m.known[id]
	if !ok {
		return
	}
	delete(m.known, id)
	if owner != "" {
		if m.perOwner[owner]--; m.perOwner[owner] <= 0 {
			delete(m.perOwner, owner)
		}
	}
}

// Close closes the underlying database.
func (m *SQLiteManager) Close() error {
	return errors.Join(m.readDB.Close(), m.db.Close())
}
