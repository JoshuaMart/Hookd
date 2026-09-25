package storage

import (
	"errors"
	"testing"
	"time"
)

func TestSQLite_CreateLongLivedHooksAllOrNothing(t *testing.T) {
	m := newTestSQLite(t, 1024)
	batch := []CreateOptions{
		{TTL: time.Hour, Metadata: map[string]any{"param": "bio"}},
		{TTL: time.Hour, Metadata: map[string]any{"param": "name"}},
	}

	hooks, err := m.CreateLongLivedHooks("example.com", batch, 3)
	if err != nil {
		t.Fatalf("CreateLongLivedHooks: %v", err)
	}
	if len(hooks) != 2 || hooks[0].Metadata["param"] != "bio" || hooks[1].Metadata["param"] != "name" {
		t.Fatalf("expected hooks in request order, got %+v", hooks)
	}

	// Two more would exceed the cap of 3: neither may be created.
	if _, err := m.CreateLongLivedHooks("example.com", batch, 3); !errors.Is(err, ErrHookLimitReached) {
		t.Fatalf("expected ErrHookLimitReached, got %v", err)
	}
	if n := m.LongLivedCount(); n != 2 {
		t.Errorf("expected the rejected batch to create nothing, got %d hooks", n)
	}
}

func TestSQLite_LongLivedHooks(t *testing.T) {
	m := newTestSQLite(t, 1024)
	first := m.CreateHook("example.com", CreateOptions{TTL: time.Hour, Metadata: map[string]any{"run": "a"}})
	second := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})

	hooks, err := m.LongLivedHooks()
	if err != nil {
		t.Fatalf("LongLivedHooks: %v", err)
	}
	if len(hooks) != 2 || hooks[0].ID != first.ID || hooks[1].ID != second.ID {
		t.Fatalf("expected both hooks oldest first, got %+v", hooks)
	}
	if hooks[0].Metadata["run"] != "a" {
		t.Errorf("expected metadata to round-trip, got %v", hooks[0].Metadata)
	}

	m.db.Close()
	if _, err := m.LongLivedHooks(); err == nil {
		t.Error("expected a storage error to surface")
	}
}

func TestMatchesMetadata(t *testing.T) {
	meta := map[string]any{"run": "0f3a", "n": float64(42), "ok": true, "nested": map[string]any{"a": "b"}}
	for _, tc := range []struct {
		filter map[string]string
		want   bool
	}{
		{nil, true},
		{map[string]string{"run": "0f3a"}, true},
		{map[string]string{"run": "0f3a", "n": "42", "ok": "true"}, true},
		{map[string]string{"run": "other"}, false},
		{map[string]string{"missing": ""}, false},
		{map[string]string{"nested": "map[a:b]"}, false},
	} {
		if got := MatchesMetadata(meta, tc.filter); got != tc.want {
			t.Errorf("MatchesMetadata(%v) = %v, want %v", tc.filter, got, tc.want)
		}
	}
	if MatchesMetadata(nil, map[string]string{"run": "0f3a"}) {
		t.Error("expected a hook without metadata not to match a filter")
	}
}
