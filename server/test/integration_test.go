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
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/jomar/hookd/internal/config"
	dnsserver "github.com/jomar/hookd/internal/dns"
	"github.com/jomar/hookd/internal/eviction"
	httpserver "github.com/jomar/hookd/internal/http"
	"github.com/jomar/hookd/internal/storage"
	"github.com/jomar/hookd/pkg/api"
)

type testServer struct {
	cfg         *config.Config
	storage     storage.Manager
	evictor     *eviction.Evictor
	dnsServer   *dnsserver.Server
	httpServer  *httpserver.Server
	ctx         context.Context
	cancel      context.CancelFunc
	idGenerator func() string
}

func setupTestServer(t *testing.T) *testServer {
	cfg := config.DefaultConfig()
	cfg.Server.Domain = "hookd.test.local"
	cfg.Server.DNS.Enabled = true
	cfg.Server.DNS.Port = 15353 // Use non-privileged port for testing
	cfg.Server.HTTP.Port = 18080
	cfg.Server.HTTPS.Enabled = false
	cfg.Server.API.AuthToken = "test-token-123"
	cfg.Eviction.InteractionTTL = 1 * time.Hour
	cfg.Eviction.MaxPerHook = 100
	cfg.Eviction.CleanupInterval = 1 * time.Second

	idCounter := 0
	idGenerator := func() string {
		idCounter++
		return fmt.Sprintf("test%d", idCounter)
	}

	storageManager := storage.NewMemoryManager(idGenerator)
	logger := setupTestLogger()
	evictor := eviction.NewEvictor(storageManager, cfg.Eviction, logger)

	ctx, cancel := context.WithCancel(context.Background())

	// Start eviction system
	go evictor.Start(ctx)

	// Start DNS server
	dnsServer, err := dnsserver.NewServer(
		cfg.Server.Domain,
		cfg.Server.DNS.Port,
		storageManager,
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
		storageManager,
		evictor,
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

func (ts *testServer) cleanup() {
	ts.cancel()
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

	hooksActive, ok := metrics["hooks_active"].(float64)
	if !ok {
		t.Error("expected hooks_active in metrics")
	}

	if int(hooksActive) != 1 {
		t.Errorf("expected 1 active hook, got %v", hooksActive)
	}

	t.Logf("Metrics: %+v", metrics)
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
				req.Header.Set("Authorization", "Bearer "+tt.token)
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
	req.Header.Set("Authorization", "Bearer "+token)

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
	req.Header.Set("Authorization", "Bearer "+token)

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

func performDNSQuery(port int, domain string) error {
	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion(domain, dns.TypeA)

	_, _, err := c.Exchange(m, fmt.Sprintf("127.0.0.1:%d", port))
	return err
}
