package http

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/eviction"
	"github.com/jomar/hookd/internal/storage"
)

func newSMTPTestHandler(t *testing.T, smtpEnabled bool) *APIHandler {
	t.Helper()

	idGen := func() string { return "abc123" }
	manager := storage.NewMemoryManager(idGen)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	evictor := eviction.NewEvictor(manager, config.EvictionConfig{
		CleanupInterval: 60,
		InteractionTTL:  3600,
		MaxPerHook:      100,
		MaxMemoryMB:     100,
	}, logger)

	return NewAPIHandler(manager, evictor, "hookd.example.com", config.LongLivedConfig{}, smtpEnabled, logger, idGen)
}

func registerOne(t *testing.T, handler *APIHandler) map[string]any {
	t.Helper()

	w := httptest.NewRecorder()
	handler.HandleRegister(w, httptest.NewRequest(http.MethodPost, "/register", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("register: status %d, body %s", w.Code, w.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	return body
}

func TestRegisterAdvertisesSMTPAddress(t *testing.T) {
	body := registerOne(t, newSMTPTestHandler(t, true))

	got, _ := body["smtp"].(string)
	if want := "abc123@hookd.example.com"; got != want {
		t.Errorf("smtp = %q, want %q", got, want)
	}
}

// With no listener running, the address would black-hole, so it is omitted
// rather than handed out.
func TestRegisterOmitsSMTPWhenDisabled(t *testing.T) {
	body := registerOne(t, newSMTPTestHandler(t, false))

	if _, present := body["smtp"]; present {
		t.Errorf("smtp field present while SMTP is disabled: %v", body["smtp"])
	}
}

func TestMetricsReportSMTPInteractions(t *testing.T) {
	idGen := func() string { return "abc123" }
	manager := storage.NewMemoryManager(idGen)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	evictor := eviction.NewEvictor(manager, config.EvictionConfig{
		CleanupInterval: 60,
		InteractionTTL:  3600,
		MaxPerHook:      100,
		MaxMemoryMB:     100,
	}, logger)
	handler := NewAPIHandler(manager, evictor, "hookd.example.com", config.LongLivedConfig{}, true, logger, idGen)

	hook := manager.CreateHook("hookd.example.com", storage.CreateOptions{SMTPEnabled: true})
	manager.AddInteraction(hook.ID, storage.SMTPInteraction("i1", "1.2.3.4", "h", "f", "r", "", "s", "b"))

	w := httptest.NewRecorder()
	handler.HandleMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("metrics: status %d", w.Code)
	}

	var body struct {
		Interactions struct {
			ByType map[string]int `json:"by_type"`
		} `json:"interactions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode metrics: %v", err)
	}

	if got := body.Interactions.ByType["smtp"]; got != 1 {
		t.Errorf("interactions.by_type.smtp = %d, want 1", got)
	}
}
