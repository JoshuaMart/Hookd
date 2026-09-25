package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// cursorStores runs a test against both backends.
func cursorStores(t *testing.T, run func(t *testing.T, m Manager)) {
	t.Run("memory", func(t *testing.T) {
		counter := 0
		run(t, NewMemoryManager(func() string { counter++; return fmt.Sprintf("hook-%d", counter) }))
	})
	t.Run("sqlite", func(t *testing.T) {
		run(t, newTestSQLite(t, 1024))
	})
}

func seqs(interactions []*Interaction) []int64 {
	out := make([]int64, len(interactions))
	for i, it := range interactions {
		out[i] = it.Seq
	}
	return out
}

func TestCursor_ReadIsNonDestructive(t *testing.T) {
	cursorStores(t, func(t *testing.T, m Manager) {
		hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
		for i := 0; i < 3; i++ {
			m.AddInteraction(hook.ID, DNSInteraction(fmt.Sprintf("i%d", i), "1.2.3.4", "q", "A"))
		}

		for range 2 {
			read, err := m.ReadInteractions(hook.ID, 0)
			if err != nil {
				t.Fatalf("ReadInteractions: %v", err)
			}
			if got := seqs(read.Interactions); fmt.Sprint(got) != "[1 2 3]" {
				t.Fatalf("expected seqs [1 2 3], got %v", got)
			}
		}

		read, _ := m.ReadInteractions(hook.ID, 2)
		if got := seqs(read.Interactions); fmt.Sprint(got) != "[3]" {
			t.Errorf("expected seqs [3] after cursor 2, got %v", got)
		}
	})
}

func TestCursor_Ack(t *testing.T) {
	cursorStores(t, func(t *testing.T, m Manager) {
		hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
		for i := 0; i < 3; i++ {
			m.AddInteraction(hook.ID, DNSInteraction(fmt.Sprintf("i%d", i), "1.2.3.4", "q", "A"))
		}

		removed, err := m.AckInteractions(hook.ID, 2)
		if err != nil || removed != 2 {
			t.Fatalf("expected 2 acknowledged, got %d (%v)", removed, err)
		}
		read, _ := m.ReadInteractions(hook.ID, 0)
		if got := seqs(read.Interactions); fmt.Sprint(got) != "[3]" {
			t.Errorf("expected seqs [3] after ack, got %v", got)
		}
		if read.DroppedThrough != 0 {
			t.Errorf("an ack is not a loss, got dropped_through %d", read.DroppedThrough)
		}

		// Seqs keep increasing after everything was acknowledged.
		m.AckInteractions(hook.ID, 3)
		m.AddInteraction(hook.ID, DNSInteraction("i3", "1.2.3.4", "q", "A"))
		read, _ = m.ReadInteractions(hook.ID, 3)
		if got := seqs(read.Interactions); fmt.Sprint(got) != "[4]" {
			t.Errorf("expected seqs [4], got %v", got)
		}
	})
}

func TestCursor_UnknownHook(t *testing.T) {
	cursorStores(t, func(t *testing.T, m Manager) {
		if _, err := m.ReadInteractions("missing", 0); !errors.Is(err, ErrHookNotFound) {
			t.Errorf("expected ErrHookNotFound on read, got %v", err)
		}
		if _, err := m.AckInteractions("missing", 1); !errors.Is(err, ErrHookNotFound) {
			t.Errorf("expected ErrHookNotFound on ack, got %v", err)
		}
	})
}

func TestCursor_PerHookLimitReportsLoss(t *testing.T) {
	cursorStores(t, func(t *testing.T, m Manager) {
		hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
		for i := 0; i < 10; i++ {
			m.AddInteraction(hook.ID, DNSInteraction(fmt.Sprintf("i%d", i), "1.2.3.4", "q", "A"))
		}

		if removed := m.EnforcePerHookLimit(4); removed != 6 {
			t.Fatalf("expected 6 removed, got %d", removed)
		}
		read, _ := m.ReadInteractions(hook.ID, 0)
		if read.DroppedThrough != 6 {
			t.Errorf("expected dropped_through 6, got %d", read.DroppedThrough)
		}
		if got := seqs(read.Interactions); fmt.Sprint(got) != "[7 8 9 10]" {
			t.Errorf("expected newest seqs kept, got %v", got)
		}
	})
}

func TestCursor_MemoryTTLEvictionReportsLoss(t *testing.T) {
	m := NewMemoryManager(func() string { return "hook" })
	m.CreateHook("example.com", CreateOptions{})

	old := DNSInteraction("old", "1.2.3.4", "q", "A")
	m.AddInteraction("hook", old)
	old.Timestamp = time.Now().Add(-2 * time.Hour)
	m.AddInteraction("hook", DNSInteraction("new", "1.2.3.4", "q", "A"))

	m.EvictInteractionsBefore(time.Now().Add(-time.Hour))
	read, _ := m.ReadInteractions("hook", 0)
	if read.DroppedThrough != 1 {
		t.Errorf("expected dropped_through 1, got %d", read.DroppedThrough)
	}
}

func TestSQLite_SeqSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ll.db")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	open := func() *SQLiteManager {
		m, err := NewSQLiteManager(path, func() string { return "hook" }, 1024, logger)
		if err != nil {
			t.Fatalf("NewSQLiteManager: %v", err)
		}
		return m
	}

	m := open()
	m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	m.AddInteraction("hook", DNSInteraction("i1", "1.2.3.4", "q", "A"))
	m.AddInteraction("hook", DNSInteraction("i2", "1.2.3.4", "q", "A"))
	m.AckInteractions("hook", 2)
	m.Close()

	// With every row acknowledged, a seq derived from the rows would restart at 1
	// and hide new interactions behind the client's cursor.
	m = open()
	defer m.Close()
	m.AddInteraction("hook", DNSInteraction("i3", "1.2.3.4", "q", "A"))
	read, _ := m.ReadInteractions("hook", 2)
	if got := seqs(read.Interactions); fmt.Sprint(got) != "[3]" {
		t.Errorf("expected seqs [3] after reopen, got %v", got)
	}
}

func TestSQLite_MigratesSeqOnExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ll.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// Schema as shipped before cursors.
	if _, err := db.Exec(`
		CREATE TABLE hooks (id TEXT PRIMARY KEY, dns TEXT NOT NULL, http TEXT NOT NULL, https TEXT NOT NULL,
			smtp TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, metadata TEXT);
		CREATE TABLE interactions (id TEXT PRIMARY KEY, hook_id TEXT NOT NULL REFERENCES hooks(id) ON DELETE CASCADE,
			type TEXT NOT NULL, timestamp INTEGER NOT NULL, source_ip TEXT, data TEXT NOT NULL);
		INSERT INTO hooks VALUES ('hook', 'd', 'h', 'hs', '', 1, 0, NULL);
		INSERT INTO interactions VALUES ('b', 'hook', 'dns', 20, '1.2.3.4', '{}');
		INSERT INTO interactions VALUES ('a', 'hook', 'dns', 10, '1.2.3.4', '{}');`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	m, err := NewSQLiteManager(path, func() string { return "x" }, 1024, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewSQLiteManager: %v", err)
	}
	defer m.Close()

	read, _ := m.ReadInteractions("hook", 0)
	if len(read.Interactions) != 2 || read.Interactions[0].ID != "a" || read.Interactions[0].Seq != 1 || read.Interactions[1].Seq != 2 {
		t.Fatalf("expected pending rows numbered in arrival order, got %+v", read.Interactions)
	}
	m.AddInteraction("hook", DNSInteraction("c", "1.2.3.4", "q", "A"))
	read, _ = m.ReadInteractions("hook", 2)
	if got := seqs(read.Interactions); fmt.Sprint(got) != "[3]" {
		t.Errorf("expected new interaction to continue at 3, got %v", got)
	}
}

func TestSQLite_DrainReturnsUnnumberedRows(t *testing.T) {
	m := newTestSQLite(t, 1024)
	hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	if _, err := m.db.Exec(
		`INSERT INTO interactions (id, hook_id, type, timestamp, source_ip, data) VALUES ('legacy', ?, 'dns', 1, '', '{}')`, hook.ID,
	); err != nil {
		t.Fatal(err)
	}
	if got := m.PollInteractions(hook.ID); len(got) != 1 || got[0].ID != "legacy" {
		t.Errorf("expected the drain to return what it deletes, got %+v", got)
	}
}

func TestSQLite_CursorErrorsSurface(t *testing.T) {
	m := newTestSQLite(t, 1024)
	hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	m.db.Close()

	if _, err := m.ReadInteractions(hook.ID, 0); err == nil || errors.Is(err, ErrHookNotFound) {
		t.Errorf("expected a storage error on read, got %v", err)
	}
	if _, err := m.AckInteractions(hook.ID, 1); err == nil || errors.Is(err, ErrHookNotFound) {
		t.Errorf("expected a storage error on ack, got %v", err)
	}
}
