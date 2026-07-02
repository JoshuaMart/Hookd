package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/eviction"
	"github.com/jomar/hookd/internal/storage"
)

const testEphemeralTTL = 24 * time.Hour

// newLongLivedHandler builds an API handler backed by a composite store with the
// long-lived feature enabled (small caps so limits are easy to exercise).
func newLongLivedHandler(t *testing.T) (*APIHandler, *storage.CompositeManager) {
	t.Helper()
	counter := 0
	idGen := func() string { counter++; return fmt.Sprintf("id-%d", counter) }

	llCfg := config.LongLivedConfig{
		Enabled:                 true,
		MaxTTL:                  720 * time.Hour,
		MaxHooks:                3,
		MaxInteractionBodyBytes: 1024,
		MaxMetadataBytes:        64,
		DBPath:                  filepath.Join(t.TempDir(), "ll.db"),
	}
	sqlite, err := storage.NewSQLiteManager(llCfg.DBPath, idGen, llCfg.MaxInteractionBodyBytes, slog.Default())
	if err != nil {
		t.Fatalf("NewSQLiteManager: %v", err)
	}
	t.Cleanup(func() { sqlite.Close() })

	composite := storage.NewCompositeManager(storage.NewMemoryManager(idGen), sqlite, testEphemeralTTL)
	evictor := eviction.NewEvictor(composite, config.EvictionConfig{
		HookTTL:         testEphemeralTTL,
		InteractionTTL:  time.Hour,
		MaxPerHook:      100,
		MaxMemoryMB:     1000,
		CleanupInterval: time.Second,
	}, slog.Default())

	handler := NewAPIHandler(composite, evictor, "example.com", llCfg, slog.Default(), idGen)
	return handler, composite
}

func registerBody(t *testing.T, h *APIHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleRegister(w, req)
	return w
}

func TestRegister_LongLived(t *testing.T) {
	handler, composite := newLongLivedHandler(t)

	w := registerBody(t, handler, `{"ttl":"720h","metadata":{"k":"v"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	var hook storage.Hook
	if err := json.NewDecoder(w.Body).Decode(&hook); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if hook.ExpiresAt.IsZero() {
		t.Error("expected expires_at to be set for long-lived hook")
	}
	if hook.Metadata["k"] != "v" {
		t.Errorf("expected metadata echoed, got %v", hook.Metadata)
	}
	if composite.LongLivedCount() != 1 {
		t.Errorf("expected 1 long-lived hook stored, got %d", composite.LongLivedCount())
	}
}

func TestRegister_TTLRejections(t *testing.T) {
	handler, _ := newLongLivedHandler(t)

	cases := []struct {
		name string
		body string
	}{
		{"ttl at or below ephemeral", `{"ttl":"12h"}`},
		{"ttl equal to ephemeral", `{"ttl":"24h"}`},
		{"invalid ttl string", `{"ttl":"soon"}`},
		{"ttl above max", `{"ttl":"1000h"}`},
		{"day count overflows int64", `{"ttl":"9223372036d"}`},
		{"negative day count", `{"ttl":"-5d"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := registerBody(t, handler, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d (%s)", w.Code, w.Body.String())
			}
		})
	}
}

func TestRegister_LongLivedDisabled(t *testing.T) {
	// Ephemeral-only handler (memory store, long-lived disabled).
	idGen := func() string { return "id-1" }
	mem := storage.NewMemoryManager(idGen)
	composite := storage.NewCompositeManager(mem, nil, testEphemeralTTL)
	evictor := eviction.NewEvictor(composite, config.EvictionConfig{HookTTL: testEphemeralTTL}, slog.Default())
	handler := NewAPIHandler(composite, evictor, "example.com", config.LongLivedConfig{Enabled: false}, slog.Default(), idGen)

	w := registerBody(t, handler, `{"ttl":"720h"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 when long-lived disabled, got %d", w.Code)
	}
}

func TestRegister_MetadataTooLarge(t *testing.T) {
	handler, _ := newLongLivedHandler(t)

	big := strings.Repeat("x", 200) // exceeds MaxMetadataBytes (64)
	w := registerBody(t, handler, fmt.Sprintf(`{"metadata":{"k":%q}}`, big))
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for oversize metadata, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestRegister_MaxHooksReturns429(t *testing.T) {
	handler, _ := newLongLivedHandler(t) // MaxHooks = 3

	// Requesting more than the cap in one shot is rejected.
	w := registerBody(t, handler, `{"count":4,"ttl":"720h"}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d (%s)", w.Code, w.Body.String())
	}

	// Filling to the cap succeeds, then one more is rejected.
	if w := registerBody(t, handler, `{"count":3,"ttl":"720h"}`); w.Code != http.StatusOK {
		t.Fatalf("expected 200 filling to cap, got %d", w.Code)
	}
	if w := registerBody(t, handler, `{"ttl":"720h"}`); w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 past cap, got %d", w.Code)
	}
}

func TestRegister_DaySuffixTTL(t *testing.T) {
	handler, composite := newLongLivedHandler(t)

	w := registerBody(t, handler, `{"ttl":"7d"}`) // 168h > 24h ephemeral, < 720h max
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for 7d ttl, got %d (%s)", w.Code, w.Body.String())
	}
	if composite.LongLivedCount() != 1 {
		t.Errorf("expected 1 long-lived hook, got %d", composite.LongLivedCount())
	}
}

func TestPoll_EchoesMetadata(t *testing.T) {
	handler, composite := newLongLivedHandler(t)

	hook := composite.CreateHook("example.com", storage.CreateOptions{
		TTL:      720 * time.Hour,
		Metadata: map[string]any{"field": "bio"},
	})
	composite.AddInteraction(hook.ID, storage.DNSInteraction("i1", "1.2.3.4", "q", "A"))

	req := httptest.NewRequest(http.MethodGet, "/poll/"+hook.ID, nil)
	w := httptest.NewRecorder()
	handler.HandlePoll(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Interactions []any          `json:"interactions"`
		Metadata     map[string]any `json:"metadata"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Metadata["field"] != "bio" {
		t.Errorf("expected metadata echoed in poll, got %v", resp.Metadata)
	}
	if len(resp.Interactions) != 1 {
		t.Errorf("expected 1 interaction, got %d", len(resp.Interactions))
	}
}

func TestActivity(t *testing.T) {
	handler, composite := newLongLivedHandler(t)

	fired := composite.CreateHook("example.com", storage.CreateOptions{TTL: 720 * time.Hour})
	composite.CreateHook("example.com", storage.CreateOptions{TTL: 720 * time.Hour}) // quiet
	composite.AddInteraction(fired.ID, storage.DNSInteraction("i1", "1.2.3.4", "q", "A"))

	t.Run("lists hooks with pending interactions", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/activity", nil)
		w := httptest.NewRecorder()
		handler.HandleActivity(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var resp struct {
			Hooks []storage.HookActivity `json:"hooks"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Hooks) != 1 {
			t.Fatalf("expected 1 active hook, got %d", len(resp.Hooks))
		}
		if resp.Hooks[0].Hook.ID != fired.ID {
			t.Errorf("expected fired hook %s, got %s", fired.ID, resp.Hooks[0].Hook.ID)
		}
		if resp.Hooks[0].PendingCount != 1 {
			t.Errorf("expected pending count 1, got %d", resp.Hooks[0].PendingCount)
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/activity", nil)
		w := httptest.NewRecorder()
		handler.HandleActivity(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected 405, got %d", w.Code)
		}
	})
}

func TestActivity_EmptyWithoutLongLivedSupport(t *testing.T) {
	// A memory-only handler does not implement LongLivedManager.
	idGen := func() string { return "id-1" }
	mem := storage.NewMemoryManager(idGen)
	evictor := eviction.NewEvictor(mem, config.EvictionConfig{HookTTL: testEphemeralTTL}, slog.Default())
	handler := NewAPIHandler(mem, evictor, "example.com", config.LongLivedConfig{}, slog.Default(), idGen)

	req := httptest.NewRequest(http.MethodGet, "/activity", nil)
	w := httptest.NewRecorder()
	handler.HandleActivity(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Hooks []storage.HookActivity `json:"hooks"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Hooks) != 0 {
		t.Errorf("expected empty activity, got %d", len(resp.Hooks))
	}
}
