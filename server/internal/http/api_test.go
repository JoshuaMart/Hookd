package http

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/eviction"
	"github.com/jomar/hookd/internal/storage"
)

func TestAPIHandler_HandleRegister(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	evictorCfg := config.EvictionConfig{
		CleanupInterval: 60,
		InteractionTTL:  3600,
		MaxPerHook:      100,
		MaxMemoryMB:     100,
	}
	evictor := eviction.NewEvictor(manager, evictorCfg, slog.Default())
	logger := slog.Default()

	handler := NewAPIHandler(manager, evictor, "example.com", logger, idGen)

	t.Run("success single hook (no body)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/register", nil)
		w := httptest.NewRecorder()

		handler.HandleRegister(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		var response storage.Hook
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if !strings.Contains(response.DNS, "example.com") {
			t.Errorf("expected DNS to contain example.com, got %s", response.DNS)
		}
	})

	t.Run("success single hook (count=1)", func(t *testing.T) {
		body := bytes.NewBufferString(`{"count": 1}`)
		req := httptest.NewRequest(http.MethodPost, "/register", body)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandleRegister(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		var response storage.Hook
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if !strings.Contains(response.DNS, "example.com") {
			t.Errorf("expected DNS to contain example.com, got %s", response.DNS)
		}
	})

	t.Run("success multiple hooks", func(t *testing.T) {
		body := bytes.NewBufferString(`{"count": 5}`)
		req := httptest.NewRequest(http.MethodPost, "/register", body)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandleRegister(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		var response map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		hooks, ok := response["hooks"].([]interface{})
		if !ok {
			t.Fatal("expected hooks array in response")
		}

		if len(hooks) != 5 {
			t.Errorf("expected 5 hooks, got %d", len(hooks))
		}

		// Verify each hook has required fields
		for i, h := range hooks {
			hook := h.(map[string]interface{})
			if _, ok := hook["id"]; !ok {
				t.Errorf("hook %d missing id field", i)
			}
			if _, ok := hook["dns"]; !ok {
				t.Errorf("hook %d missing dns field", i)
			}
		}
	})

	t.Run("invalid count (zero)", func(t *testing.T) {
		body := bytes.NewBufferString(`{"count": 0}`)
		req := httptest.NewRequest(http.MethodPost, "/register", body)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandleRegister(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200 (defaults to 1), got %d", w.Code)
		}

		// Should default to 1 hook
		var response storage.Hook
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
	})

	t.Run("invalid count (negative)", func(t *testing.T) {
		body := bytes.NewBufferString(`{"count": -5}`)
		req := httptest.NewRequest(http.MethodPost, "/register", body)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandleRegister(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200 (defaults to 1), got %d", w.Code)
		}

		// Should default to 1 hook
		var response storage.Hook
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
	})

	t.Run("invalid json body", func(t *testing.T) {
		body := bytes.NewBufferString(`{invalid json}`)
		req := httptest.NewRequest(http.MethodPost, "/register", body)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandleRegister(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200 (defaults to 1), got %d", w.Code)
		}

		// Should default to 1 hook when JSON parsing fails
		var response storage.Hook
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/register", nil)
		w := httptest.NewRecorder()

		handler.HandleRegister(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected status 405, got %d", w.Code)
		}
	})
}

func TestAPIHandler_HandlePoll(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	evictorCfg := config.EvictionConfig{
		CleanupInterval: 60,
		InteractionTTL:  3600,
		MaxPerHook:      100,
		MaxMemoryMB:     100,
	}
	evictor := eviction.NewEvictor(manager, evictorCfg, slog.Default())
	logger := slog.Default()

	handler := NewAPIHandler(manager, evictor, "example.com", logger, idGen)

	// Create a hook first
	hook := manager.CreateHook("example.com")

	t.Run("success with no interactions", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/poll/"+hook.ID, nil)
		w := httptest.NewRecorder()

		handler.HandlePoll(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		var response map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		interactions := response["interactions"].([]interface{})
		if len(interactions) != 0 {
			t.Errorf("expected 0 interactions, got %d", len(interactions))
		}
	})

	t.Run("success with interactions", func(t *testing.T) {
		// Add an interaction
		interaction := storage.HTTPInteraction("int-1", "1.2.3.4", "GET", "/test", map[string]string{}, "")
		manager.AddInteraction(hook.ID, interaction)

		req := httptest.NewRequest(http.MethodGet, "/poll/"+hook.ID, nil)
		w := httptest.NewRecorder()

		handler.HandlePoll(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		var response map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		interactions := response["interactions"].([]interface{})
		if len(interactions) != 1 {
			t.Errorf("expected 1 interaction, got %d", len(interactions))
		}
	})

	t.Run("hook not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/poll/nonexistent", nil)
		w := httptest.NewRecorder()

		handler.HandlePoll(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected status 404, got %d", w.Code)
		}
	})

	t.Run("invalid path", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/poll", nil)
		w := httptest.NewRecorder()

		handler.HandlePoll(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d", w.Code)
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/poll/"+hook.ID, nil)
		w := httptest.NewRecorder()

		handler.HandlePoll(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected status 405, got %d", w.Code)
		}
	})
}

func TestAPIHandler_HandleMetrics(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	evictorCfg := config.EvictionConfig{
		CleanupInterval: 60,
		InteractionTTL:  3600,
		MaxPerHook:      100,
		MaxMemoryMB:     100,
	}
	evictor := eviction.NewEvictor(manager, evictorCfg, slog.Default())
	logger := slog.Default()

	handler := NewAPIHandler(manager, evictor, "example.com", logger, idGen)

	t.Run("success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		w := httptest.NewRecorder()

		handler.HandleMetrics(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		var response map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		// Check new nested structure
		if _, ok := response["hooks"]; !ok {
			t.Error("expected hooks section in response")
		}

		hooks := response["hooks"].(map[string]interface{})
		if _, ok := hooks["active"]; !ok {
			t.Error("expected hooks.active in response")
		}

		if _, ok := response["interactions"]; !ok {
			t.Error("expected interactions section in response")
		}

		interactions := response["interactions"].(map[string]interface{})
		if _, ok := interactions["total"]; !ok {
			t.Error("expected interactions.total in response")
		}

		if _, ok := interactions["by_type"]; !ok {
			t.Error("expected interactions.by_type in response")
		}

		if _, ok := response["evictions"]; !ok {
			t.Error("expected evictions section in response")
		}

		evictions := response["evictions"].(map[string]interface{})
		if _, ok := evictions["total"]; !ok {
			t.Error("expected evictions.total in response")
		}

		if _, ok := evictions["by_strategy"]; !ok {
			t.Error("expected evictions.by_strategy in response")
		}

		if _, ok := response["memory"]; !ok {
			t.Error("expected memory section in response")
		}

		// Check detailed memory metrics
		memory := response["memory"].(map[string]interface{})
		expectedMemoryFields := []string{"alloc_mb", "heap_inuse_mb", "sys_mb", "gc_runs"}
		for _, field := range expectedMemoryFields {
			if _, ok := memory[field]; !ok {
				t.Errorf("expected memory.%s in response", field)
			}
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/metrics", nil)
		w := httptest.NewRecorder()

		handler.HandleMetrics(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected status 405, got %d", w.Code)
		}
	})
}

func TestCaptureHandler_ServeHTTP(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	logger := slog.Default()

	handler := NewCaptureHandler(manager, "example.com", logger, idGen)

	// Create a hook first
	hook := manager.CreateHook("example.com")

	t.Run("capture http interaction", func(t *testing.T) {
		body := bytes.NewBufferString(`{"test": "data"}`)
		req := httptest.NewRequest(http.MethodPost, "/test", body)
		req.Host = hook.ID + ".example.com"
		req.Header.Set("X-Custom", "test")

		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		// Verify interaction was stored
		interactions, _ := manager.PollInteractions(hook.ID)
		if len(interactions) != 1 {
			t.Fatalf("expected 1 interaction, got %d", len(interactions))
		}

		if interactions[0].Type != storage.InteractionTypeHTTP {
			t.Errorf("expected type http, got %s", interactions[0].Type)
		}
	})

	t.Run("invalid subdomain", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "invalid.com"

		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}
	})

	t.Run("exact domain", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "example.com"

		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}
	})
}

func TestCaptureHandler_ExtractHookID(t *testing.T) {
	handler := &CaptureHandler{domain: "example.com"}

	tests := []struct {
		name     string
		host     string
		expected string
	}{
		{"valid subdomain", "abc123.example.com", "abc123"},
		{"with port", "abc123.example.com:8080", "abc123"},
		{"exact domain", "example.com", ""},
		{"invalid domain", "other.com", ""},
		{"multi-level subdomain", "sub.abc123.example.com", "sub"},
		{"no subdomain", "example.com:80", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := handler.extractHookID(tt.host)
			if result != tt.expected {
				t.Errorf("extractHookID(%q) = %q, want %q", tt.host, result, tt.expected)
			}
		})
	}
}

func TestExtractIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		expected   string
	}{
		{"ipv4 with port", "192.168.1.1:12345", "192.168.1.1"},
		{"ipv6 with port", "[::1]:8080", "[::1]"},
		{"no port", "192.168.1.1", "192.168.1.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractIP(tt.remoteAddr)
			if result != tt.expected {
				t.Errorf("extractIP(%q) = %q, want %q", tt.remoteAddr, result, tt.expected)
			}
		})
	}
}

func TestRespondJSON(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		w := httptest.NewRecorder()
		data := map[string]string{"key": "value"}

		respondJSON(w, http.StatusOK, data)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", ct)
		}

		var result map[string]string
		if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if result["key"] != "value" {
			t.Errorf("expected key=value, got key=%s", result["key"])
		}
	})

	t.Run("with error status", func(t *testing.T) {
		w := httptest.NewRecorder()
		data := map[string]string{"error": "test error"}

		respondJSON(w, http.StatusBadRequest, data)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d", w.Code)
		}
	})
}

func TestAPIHandler_HandlePoll_EdgeCases(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	evictorCfg := config.EvictionConfig{
		CleanupInterval: 60,
		InteractionTTL:  3600,
		MaxPerHook:      100,
		MaxMemoryMB:     100,
	}
	evictor := eviction.NewEvictor(manager, evictorCfg, slog.Default())
	logger := slog.Default()

	handler := NewAPIHandler(manager, evictor, "example.com", logger, idGen)

	t.Run("empty path segments", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/poll//", nil)
		w := httptest.NewRecorder()

		handler.HandlePoll(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d", w.Code)
		}
	})
}

func TestCaptureHandler_LargeBody(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	logger := slog.Default()

	handler := NewCaptureHandler(manager, "example.com", logger, idGen)
	hook := manager.CreateHook("example.com")

	// Create large body (11MB - over 10MB limit)
	largeBody := bytes.Repeat([]byte("x"), 11*1024*1024)

	req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewReader(largeBody))
	req.Host = hook.ID + ".example.com"

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	// Verify interaction was stored (body should be truncated)
	interactions, _ := manager.PollInteractions(hook.ID)
	if len(interactions) != 1 {
		t.Fatalf("expected 1 interaction, got %d", len(interactions))
	}
}

func TestAPIHandler_HandlePollBatch(t *testing.T) {
	idCounter := 0
	idGen := func() string {
		idCounter++
		return "test-id-" + string(rune('a'+idCounter-1))
	}
	manager := storage.NewMemoryManager(idGen)
	evictorCfg := config.EvictionConfig{
		CleanupInterval: 60,
		InteractionTTL:  3600,
		MaxPerHook:      100,
		MaxMemoryMB:     100,
	}
	evictor := eviction.NewEvictor(manager, evictorCfg, slog.Default())
	logger := slog.Default()

	handler := NewAPIHandler(manager, evictor, "example.com", logger, idGen)

	// Create multiple hooks
	hook1 := manager.CreateHook("example.com")
	hook2 := manager.CreateHook("example.com")
	hook3 := manager.CreateHook("example.com")

	// Add interactions to hook1
	manager.AddInteraction(hook1.ID, storage.DNSInteraction(idGen(), "1.2.3.4", "test.example.com", "A"))
	manager.AddInteraction(hook1.ID, storage.HTTPInteraction(idGen(), "5.6.7.8", "GET", "/test", nil, ""))

	// Add interactions to hook2
	manager.AddInteraction(hook2.ID, storage.DNSInteraction(idGen(), "9.10.11.12", "test2.example.com", "AAAA"))

	// hook3 has no interactions

	t.Run("success with multiple hooks", func(t *testing.T) {
		reqBody := map[string]interface{}{
			"hook_ids": []string{hook1.ID, hook2.ID, hook3.ID},
		}
		body, _ := json.Marshal(reqBody)
		req := httptest.NewRequest(http.MethodPost, "/poll", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandlePollBatch(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		var response map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		results, ok := response["results"].(map[string]interface{})
		if !ok {
			t.Fatal("expected results map in response")
		}

		// Check hook1 results (should have 2 interactions)
		hook1Result, ok := results[hook1.ID].(map[string]interface{})
		if !ok {
			t.Fatal("expected hook1 result")
		}
		hook1Interactions, ok := hook1Result["interactions"].([]interface{})
		if !ok {
			t.Fatal("expected interactions array for hook1")
		}
		if len(hook1Interactions) != 2 {
			t.Errorf("expected 2 interactions for hook1, got %d", len(hook1Interactions))
		}

		// Check hook2 results (should have 1 interaction)
		hook2Result, ok := results[hook2.ID].(map[string]interface{})
		if !ok {
			t.Fatal("expected hook2 result")
		}
		hook2Interactions, ok := hook2Result["interactions"].([]interface{})
		if !ok {
			t.Fatal("expected interactions array for hook2")
		}
		if len(hook2Interactions) != 1 {
			t.Errorf("expected 1 interaction for hook2, got %d", len(hook2Interactions))
		}

		// Check hook3 results (should have 0 interactions)
		hook3Result, ok := results[hook3.ID].(map[string]interface{})
		if !ok {
			t.Fatal("expected hook3 result")
		}
		hook3Interactions, ok := hook3Result["interactions"].([]interface{})
		if !ok {
			// Debug: print the actual result
			t.Logf("hook3Result: %+v", hook3Result)
			t.Logf("hook3 interactions value: %v (type: %T)", hook3Result["interactions"], hook3Result["interactions"])
			t.Fatal("expected interactions array for hook3")
		}
		if len(hook3Interactions) != 0 {
			t.Errorf("expected 0 interactions for hook3, got %d", len(hook3Interactions))
		}

		// Verify interactions were deleted (atomic poll)
		remaining1, _ := manager.PollInteractions(hook1.ID)
		if len(remaining1) != 0 {
			t.Errorf("expected hook1 interactions to be cleared, got %d", len(remaining1))
		}
	})

	t.Run("success with non-existent hook", func(t *testing.T) {
		reqBody := map[string]interface{}{
			"hook_ids": []string{"nonexistent", hook1.ID},
		}
		body, _ := json.Marshal(reqBody)
		req := httptest.NewRequest(http.MethodPost, "/poll", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandlePollBatch(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		var response map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		results, ok := response["results"].(map[string]interface{})
		if !ok {
			t.Fatal("expected results map in response")
		}

		// Check nonexistent hook has error
		nonexistentResult, ok := results["nonexistent"].(map[string]interface{})
		if !ok {
			t.Fatal("expected nonexistent hook result")
		}
		errorMsg, ok := nonexistentResult["error"].(string)
		if !ok || errorMsg != "Hook not found" {
			t.Errorf("expected 'Hook not found' error, got %v", nonexistentResult)
		}
	})

	t.Run("empty hook_ids array", func(t *testing.T) {
		reqBody := map[string]interface{}{
			"hook_ids": []string{},
		}
		body, _ := json.Marshal(reqBody)
		req := httptest.NewRequest(http.MethodPost, "/poll", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandlePollBatch(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d", w.Code)
		}
	})

	t.Run("invalid json body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/poll", bytes.NewBufferString("{invalid json}"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandlePollBatch(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d", w.Code)
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/poll", nil)
		w := httptest.NewRecorder()

		handler.HandlePollBatch(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected status 405, got %d", w.Code)
		}
	})
}
