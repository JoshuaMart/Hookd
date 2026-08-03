//go:build integration
// +build integration

package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/jomar/hookd/internal/acme"
	"github.com/jomar/hookd/internal/config"
	dnsserver "github.com/jomar/hookd/internal/dns"
	"github.com/jomar/hookd/internal/eviction"
	httpserver "github.com/jomar/hookd/internal/http"
	"github.com/jomar/hookd/internal/storage"
	"github.com/jomar/hookd/pkg/api"
)

type testServer struct {
	cfg         *config.Config
	storage     *storage.CompositeManager
	evictor     *eviction.Evictor
	dnsServer   *dnsserver.Server
	httpServer  *httpserver.Server
	ctx         context.Context
	cancel      context.CancelFunc
	idGenerator func() string
}

// defaultTestConfig returns a config wired for the local test ports, with the
// long-lived store persisted under dbPath.
func defaultTestConfig(dbPath string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Server.Domain = "hookd.test.local"
	cfg.Server.DNS.Enabled = true
	cfg.Server.DNS.Port = 15353 // Use non-privileged port for testing
	// Pin both to loopback: the listener has to be where the test queries it, and
	// an explicit public IP keeps A answers off the auto-detection path, which
	// needs a route to the outside.
	cfg.Server.DNS.BindAddress = "127.0.0.1"
	cfg.Server.PublicIP = "127.0.0.1"
	cfg.Server.HTTP.Port = 18080
	cfg.Server.HTTPS.Enabled = false
	cfg.Server.API.AuthToken = "test-token-123"
	cfg.Eviction.InteractionTTL = 1 * time.Hour
	cfg.Eviction.MaxPerHook = 100
	cfg.Eviction.CleanupInterval = 1 * time.Second
	cfg.LongLived.DBPath = dbPath
	return cfg
}

// startServer builds the storage stack and starts the DNS/HTTP servers for cfg.
// The long-lived store persists to cfg.LongLived.DBPath, so calling this twice
// with the same path (and a fresh in-memory store) simulates a restart.
func startServer(t *testing.T, cfg *config.Config, idGenerator func() string) *testServer {
	logger := setupTestLogger()

	memoryManager := storage.NewMemoryManager(idGenerator)
	var longLived *storage.SQLiteManager
	if cfg.LongLived.Enabled {
		var err error
		longLived, err = storage.NewSQLiteManager(cfg.LongLived.DBPath, idGenerator, cfg.LongLived.MaxInteractionBodyBytes, logger)
		if err != nil {
			t.Fatalf("failed to open long-lived store: %v", err)
		}
	}
	storageManager := storage.NewCompositeManager(memoryManager, longLived, cfg.Eviction.HookTTL)

	evictor := eviction.NewEvictor(storageManager, cfg.Eviction, logger)
	acmeProvider := acme.NewProvider(logger)

	ctx, cancel := context.WithCancel(context.Background())

	// Start eviction system
	go evictor.Start(ctx)

	// Start DNS server
	dnsServer, err := dnsserver.NewServer(
		cfg.Server.Domain,
		cfg.Server.DNS.Port,
		cfg.Server.PublicIP,
		cfg.Server.DNS.BindAddress,
		storageManager,
		acmeProvider,
		logger,
		idGenerator,
	)
	if err != nil {
		t.Fatalf("failed to create DNS server: %v", err)
	}

	go func() {
		if err := dnsServer.Start(ctx); err != nil {
			t.Logf("DNS server error: %v", err)
		}
	}()

	// Start HTTP server
	httpServer := httpserver.NewServer(
		cfg.Server,
		cfg.LongLived,
		cfg.Observability,
		storageManager,
		evictor,
		acmeProvider,
		logger,
		idGenerator,
	)

	go func() {
		if err := httpServer.Start(ctx); err != nil {
			t.Logf("HTTP server error: %v", err)
		}
	}()

	// Give servers time to start
	time.Sleep(100 * time.Millisecond)

	return &testServer{
		cfg:         cfg,
		storage:     storageManager,
		evictor:     evictor,
		dnsServer:   dnsServer,
		httpServer:  httpServer,
		ctx:         ctx,
		cancel:      cancel,
		idGenerator: idGenerator,
	}
}

func setupTestServer(t *testing.T) *testServer {
	idCounter := 0
	idGenerator := func() string {
		idCounter++
		return fmt.Sprintf("test%d", idCounter)
	}
	cfg := defaultTestConfig(filepath.Join(t.TempDir(), "longlived.db"))
	return startServer(t, cfg, idGenerator)
}

func (ts *testServer) cleanup() {
	ts.cancel()
	if ts.storage != nil {
		ts.storage.Close()
	}
	time.Sleep(100 * time.Millisecond)
}

func setupTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestIntegration_RegisterAndPoll(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	// Register a hook
	hook, err := registerHook(ts.cfg.Server.HTTP.Port, ts.cfg.Server.API.AuthToken)
	if err != nil {
		t.Fatalf("failed to register hook: %v", err)
	}

	if hook.ID == "" {
		t.Error("expected hook ID to be set")
	}

	t.Logf("Registered hook: %s", hook.ID)

	// Poll (should be empty)
	interactions, err := pollHook(ts.cfg.Server.HTTP.Port, ts.cfg.Server.API.AuthToken, hook.ID)
	if err != nil {
		t.Fatalf("failed to poll hook: %v", err)
	}

	if len(interactions) != 0 {
		t.Errorf("expected 0 interactions, got %d", len(interactions))
	}
}

func TestIntegration_DNSInteraction(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	// Register a hook
	hook, err := registerHook(ts.cfg.Server.HTTP.Port, ts.cfg.Server.API.AuthToken)
	if err != nil {
		t.Fatalf("failed to register hook: %v", err)
	}

	t.Logf("Registered hook: %s", hook.ID)

	// Perform DNS query
	queryDomain := hook.ID + "." + ts.cfg.Server.Domain + "."
	err = performDNSQuery(ts.cfg.Server.DNS.Port, queryDomain)
	if err != nil {
		t.Fatalf("failed to perform DNS query: %v", err)
	}

	// Wait a bit for interaction to be stored
	time.Sleep(50 * time.Millisecond)

	// Poll interactions
	interactions, err := pollHook(ts.cfg.Server.HTTP.Port, ts.cfg.Server.API.AuthToken, hook.ID)
	if err != nil {
		t.Fatalf("failed to poll hook: %v", err)
	}

	if len(interactions) != 1 {
		t.Fatalf("expected 1 interaction, got %d", len(interactions))
	}

	if interactions[0].Type != "dns" {
		t.Errorf("expected dns interaction, got %s", interactions[0].Type)
	}

	dnsData := interactions[0].Data
	if dnsData["qname"] != queryDomain {
		t.Errorf("expected qname %s, got %v", queryDomain, dnsData["qname"])
	}
}

func TestIntegration_HTTPInteraction(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	// Register a hook
	hook, err := registerHook(ts.cfg.Server.HTTP.Port, ts.cfg.Server.API.AuthToken)
	if err != nil {
		t.Fatalf("failed to register hook: %v", err)
	}

	t.Logf("Registered hook: %s", hook.ID)

	// Perform HTTP request
	url := fmt.Sprintf("http://localhost:%d/callback", ts.cfg.Server.HTTP.Port)
	req, _ := http.NewRequest("POST", url, bytes.NewBufferString("test payload"))
	req.Host = hook.ID + "." + ts.cfg.Server.Domain
	req.Header.Set("User-Agent", "test-agent")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("failed to perform HTTP request: %v", err)
	}
	resp.Body.Close()

	// Wait a bit for interaction to be stored
	time.Sleep(50 * time.Millisecond)

	// Poll interactions
	interactions, err := pollHook(ts.cfg.Server.HTTP.Port, ts.cfg.Server.API.AuthToken, hook.ID)
	if err != nil {
		t.Fatalf("failed to poll hook: %v", err)
	}

	if len(interactions) != 1 {
		t.Fatalf("expected 1 interaction, got %d", len(interactions))
	}

	if interactions[0].Type != "http" {
		t.Errorf("expected http interaction, got %s", interactions[0].Type)
	}

	httpData := interactions[0].Data
	if httpData["method"] != "POST" {
		t.Errorf("expected method POST, got %v", httpData["method"])
	}

	if httpData["body"] != "test payload" {
		t.Errorf("expected body 'test payload', got %v", httpData["body"])
	}
}

func TestIntegration_Metrics(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	// Register a hook
	_, err := registerHook(ts.cfg.Server.HTTP.Port, ts.cfg.Server.API.AuthToken)
	if err != nil {
		t.Fatalf("failed to register hook: %v", err)
	}

	// Get metrics
	url := fmt.Sprintf("http://localhost:%d/metrics", ts.cfg.Server.HTTP.Port)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("failed to get metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	var metrics map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&metrics); err != nil {
		t.Fatalf("failed to decode metrics: %v", err)
	}

	hooks, ok := metrics["hooks"].(map[string]interface{})
	if !ok {
		t.Fatal("expected hooks section in metrics")
	}
	hooksActive, ok := hooks["active"].(float64)
	if !ok {
		t.Error("expected hooks.active in metrics")
	}

	if int(hooksActive) != 1 {
		t.Errorf("expected 1 active hook, got %v", hooksActive)
	}

	t.Logf("Metrics: %+v", metrics)
}

func TestIntegration_LongLivedSurvivesRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "longlived.db")
	idCounter := 0
	idGenerator := func() string {
		idCounter++
		return fmt.Sprintf("test%d", idCounter)
	}

	// First lifecycle: register a long-lived hook, then shut down.
	ts1 := startServer(t, defaultTestConfig(dbPath), idGenerator)
	hook, err := registerLongLived(ts1.cfg.Server.HTTP.Port, ts1.cfg.Server.API.AuthToken, "720h",
		map[string]any{"field": "profile.bio"})
	if err != nil {
		t.Fatalf("failed to register long-lived hook: %v", err)
	}
	if hook.ExpiresAt.IsZero() {
		t.Error("expected long-lived hook to have an expiry")
	}
	ts1.cleanup()

	// Second lifecycle: fresh in-memory store, the SAME database, on different
	// ports (avoids reusing a port the first instance just released).
	cfg2 := defaultTestConfig(dbPath)
	cfg2.Server.DNS.Port = 15354
	cfg2.Server.HTTP.Port = 18081
	ts2 := startServer(t, cfg2, idGenerator)
	defer ts2.cleanup()

	// The stored-XSS payload fires only now, after the restart. A purely
	// in-memory server would have forgotten the hook and dropped this silently.
	queryDomain := hook.ID + "." + cfg2.Server.Domain + "."
	if err := performDNSQuery(cfg2.Server.DNS.Port, queryDomain); err != nil {
		t.Fatalf("failed to perform DNS query: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// It shows up in the activity list...
	activity, err := getActivity(cfg2.Server.HTTP.Port, cfg2.Server.API.AuthToken)
	if err != nil {
		t.Fatalf("failed to get activity: %v", err)
	}
	if len(activity) != 1 || activity[0].Hook.ID != hook.ID {
		t.Fatalf("expected activity to list the surviving hook, got %+v", activity)
	}
	if activity[0].Hook.Metadata["field"] != "profile.bio" {
		t.Errorf("expected metadata preserved across restart, got %v", activity[0].Hook.Metadata)
	}

	// ...and draining it returns the captured interaction plus its metadata.
	resp, err := pollFull(cfg2.Server.HTTP.Port, cfg2.Server.API.AuthToken, hook.ID)
	if err != nil {
		t.Fatalf("failed to poll: %v", err)
	}
	if len(resp.Interactions) != 1 {
		t.Fatalf("expected 1 interaction after restart, got %d", len(resp.Interactions))
	}
	if resp.Interactions[0].Type != "dns" {
		t.Errorf("expected dns interaction, got %s", resp.Interactions[0].Type)
	}
	if resp.Metadata["field"] != "profile.bio" {
		t.Errorf("expected metadata echoed on poll, got %v", resp.Metadata)
	}
}

func TestIntegration_Authentication(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	tests := []struct {
		name       string
		token      string
		wantStatus int
	}{
		{
			name:       "valid token",
			token:      "test-token-123",
			wantStatus: http.StatusOK,
		},
		{
			name:       "invalid token",
			token:      "wrong-token",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing token",
			token:      "",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := fmt.Sprintf("http://localhost:%d/register", ts.cfg.Server.HTTP.Port)
			req, _ := http.NewRequest("POST", url, nil)

			if tt.token != "" {
				req.Header.Set("X-API-Key", tt.token)
			}

			client := &http.Client{}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("failed to make request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("expected status %d, got %d", tt.wantStatus, resp.StatusCode)
			}
		})
	}
}

// Helper functions

func registerHook(port int, token string) (*api.Hook, error) {
	url := fmt.Sprintf("http://localhost:%d/register", port)
	req, _ := http.NewRequest("POST", url, nil)
	req.Header.Set("X-API-Key", token)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var hook api.Hook
	if err := json.NewDecoder(resp.Body).Decode(&hook); err != nil {
		return nil, err
	}

	return &hook, nil
}

func pollHook(port int, token, hookID string) ([]api.Interaction, error) {
	url := fmt.Sprintf("http://localhost:%d/poll/%s", port, hookID)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("X-API-Key", token)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var result api.PollResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Interactions, nil
}

func registerLongLived(port int, token, ttl string, metadata map[string]any) (*api.Hook, error) {
	body, _ := json.Marshal(api.RegisterRequest{TTL: ttl, Metadata: metadata})
	url := fmt.Sprintf("http://localhost:%d/register", port)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("X-API-Key", token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(b))
	}

	var hook api.Hook
	if err := json.NewDecoder(resp.Body).Decode(&hook); err != nil {
		return nil, err
	}
	return &hook, nil
}

func getActivity(port int, token string) ([]api.HookActivity, error) {
	url := fmt.Sprintf("http://localhost:%d/activity", port)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("X-API-Key", token)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(b))
	}

	var result api.ActivityResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Hooks, nil
}

func pollFull(port int, token, hookID string) (*api.PollResponse, error) {
	url := fmt.Sprintf("http://localhost:%d/poll/%s", port, hookID)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("X-API-Key", token)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(b))
	}

	var result api.PollResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func performDNSQuery(port int, domain string) error {
	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion(domain, dns.TypeA)

	_, _, err := c.Exchange(m, fmt.Sprintf("127.0.0.1:%d", port))
	return err
}
