package storage

import (
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLiteStartupFailures(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		prepare     func(*testing.T, string) string
	}{
		{"parent is a file", "create db directory", func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "file")
			if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(p, "db")
		}},
		{"corrupt database", "apply schema", func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "db")
			if err := os.WriteFile(p, []byte(strings.Repeat("not sqlite", 100)), 0600); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"incompatible hook index", "load hook index", func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "db")
			db, err := sql.Open("sqlite", p)
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.Exec(`CREATE TABLE hooks (expires_at INTEGER)`)
			db.Close()
			if err != nil {
				t.Fatal(err)
			}
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.prepare(t, t.TempDir())
			m, err := NewSQLiteManager(path, func() string { return "hook" }, 1024, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if m != nil {
				m.Close()
				t.Fatal("startup returned a manager on failure")
			}
			if err == nil || !strings.Contains(err.Error(), tc.stage) {
				t.Fatalf("expected %s error, got %v", tc.stage, err)
			}
		})
	}
}

func TestSQLiteReaderInitializationFailure(t *testing.T) {
	db, err := openSQLiteReaders("file:" + filepath.Join(t.TempDir(), "missing", "db"))
	if db != nil {
		db.Close()
		t.Fatal("invalid reader pool returned")
	}
	if err == nil || !strings.Contains(err.Error(), "initialize sqlite readers") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSQLiteClosedStoreReportsFailures(t *testing.T) {
	m := newTestSQLite(t, 1024)
	hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.loadIndex(); err == nil {
		t.Fatal("closed index load succeeded")
	}
	if got, ok := m.GetHook(hook.ID); ok || got != nil {
		t.Fatal("closed store returned a hook")
	}
	if _, err := m.CreateLongLivedHook("example.com", CreateOptions{}, 0); err == nil {
		t.Fatal("closed store accepted registration")
	}
	fallback := m.CreateHook("example.com", CreateOptions{})
	if fallback == nil || m.Has(fallback.ID) {
		t.Fatal("failed legacy registration changed index")
	}
	if n := len(m.PollInteractions(hook.ID)); n != 0 {
		t.Fatalf("closed poll returned %d interactions", n)
	}
	if _, err := m.pollChunk([]string{hook.ID}); err == nil {
		t.Fatal("closed batch poll succeeded")
	}
	if _, err := m.ReadInteractions(hook.ID, 0); err == nil {
		t.Fatal("closed read succeeded")
	}
	if _, err := m.AckInteractions(hook.ID, 1); err == nil {
		t.Fatal("closed ack succeeded")
	}
	if _, err := m.trimHook(hook.ID, 0); err == nil {
		t.Fatal("closed trim succeeded")
	}
	if n := m.EvictExpiredHooks(time.Now().Add(2 * time.Hour)); n != 0 {
		t.Fatal("reported unsuccessful expiry")
	}
	if n := m.EnforcePerHookLimit(0); n != 0 {
		t.Fatal("reported unsuccessful trim")
	}
	if activity := m.LongLivedActivity(); activity != nil {
		t.Fatal("closed store returned activity")
	}
	if stats := m.Stats(); stats.InteractionsTotal != 0 || stats.HooksActive != 1 {
		t.Fatalf("unexpected closed stats: %+v", stats)
	}
	m.AddInteraction(hook.ID, DNSInteraction("closed", "127.0.0.1", "q", "A"))
}

func TestSQLitePollFailureKeepsInteractions(t *testing.T) {
	for _, failure := range []string{"decode", "delete"} {
		t.Run(failure, func(t *testing.T) {
			m := newTestSQLite(t, 1024)
			hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
			m.AddInteraction(hook.ID, DNSInteraction("one", "127.0.0.1", "q", "A"))
			var err error
			if failure == "decode" {
				_, err = m.db.Exec(`UPDATE interactions SET data='not JSON'`)
			} else {
				_, err = m.db.Exec(`CREATE TRIGGER reject_delete BEFORE DELETE ON interactions BEGIN SELECT RAISE(ABORT,'delete failed'); END`)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := m.PollInteractions(hook.ID); len(got) != 0 {
				t.Fatalf("failed poll returned data: %v", got)
			}
			var count int
			if err := m.db.QueryRow(`SELECT COUNT(*) FROM interactions`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("data lost after failed poll: count=%d, err=%v", count, err)
			}
			if failure == "decode" {
				_, err = m.db.Exec(`UPDATE interactions SET data='{}'`)
			} else {
				_, err = m.db.Exec(`DROP TRIGGER reject_delete`)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := m.PollInteractions(hook.ID); len(got) != 1 || got[0].ID != "one" {
				t.Fatalf("retry did not recover interaction: %v", got)
			}
		})
	}
}

func TestSQLiteInvalidInteractionDoesNotAdvanceCursor(t *testing.T) {
	m := newTestSQLite(t, 1024)
	hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
	invalid := DNSInteraction("invalid", "127.0.0.1", "q", "A")
	invalid.Data["invalid"] = make(chan int)
	m.AddInteraction(hook.ID, invalid)
	m.AddInteraction(hook.ID, DNSInteraction("valid", "127.0.0.1", "q", "A"))
	read, err := m.ReadInteractions(hook.ID, 0)
	if err != nil || len(read.Interactions) != 1 || read.Interactions[0].Seq != 1 {
		t.Fatalf("invalid insertion affected cursor: %+v, %v", read, err)
	}
}
