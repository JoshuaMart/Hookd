//go:build integration
// +build integration

package test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/jomar/hookd/internal/acme"
	"github.com/jomar/hookd/internal/config"
	dnsserver "github.com/jomar/hookd/internal/dns"
	"github.com/jomar/hookd/internal/eviction"
	httpserver "github.com/jomar/hookd/internal/http"
	smtpserver "github.com/jomar/hookd/internal/smtp"
	"github.com/jomar/hookd/internal/storage"
	"github.com/jomar/hookd/pkg/api"
)

type testServer struct {
	cfg         *config.Config
	storage     *storage.CompositeManager
	evictor     *eviction.Evictor
	dnsServer   *dnsserver.Server
	httpServer  *httpserver.Server
	smtpServer  *smtpserver.Server
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
	// Loopback both: the listener must be where the test queries it, and an
	// explicit public IP keeps startup off the auto-detection path.
	cfg.Server.DNS.BindAddress = "127.0.0.1"
	cfg.Server.PublicIP = "127.0.0.1"
	cfg.Server.HTTP.Port = 18080
	cfg.Server.HTTPS.Enabled = false
	// Enabled so a registered hook advertises its address; tests opt out by
	// clearing it before startServer.
	cfg.Server.SMTP.Enabled = true
	cfg.Server.SMTP.Port = 12525
	cfg.Server.SMTP.BindAddress = "127.0.0.1"
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

	// Start SMTP server
	var smtpServer *smtpserver.Server
	if cfg.Server.SMTP.Enabled {
		smtpServer, err = smtpserver.NewServer(
			cfg.Server.Domain,
			cfg.Server.SMTP,
			cfg.Eviction.MaxInteractionBodyBytes,
			storageManager,
			logger,
			idGenerator,
		)
		if err != nil {
			t.Fatalf("failed to create SMTP server: %v", err)
		}

		go func() {
			if err := smtpServer.Start(ctx); err != nil {
				t.Logf("SMTP server error: %v", err)
			}
		}()
	}

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
		smtpServer:  smtpServer,
		ctx:         ctx,
		cancel:      cancel,
		idGenerator: idGenerator,
	}
}

func setupTestServer(t *testing.T) *testServer {
	idGenerator := sequentialIDs()
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

// sequentialIDs returns a deterministic ID generator. It is shared by every
// listener, each serving on its own goroutine, so the counter is guarded.
func sequentialIDs() func() string {
	var (
		mu      sync.Mutex
		counter int
	)
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		counter++
		return fmt.Sprintf("test%d", counter)
	}
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
	idGenerator := sequentialIDs()

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

// deliverMail runs a whole SMTP transaction against the test listener and
// returns the final reply.
func deliverMail(port int, from, to, message string) (string, error) {
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", err
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return "", err
	}
	br := bufio.NewReader(conn)

	readReply := func() (string, error) {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return "", err
			}
			line = strings.TrimRight(line, "\r\n")
			// Skip continuation lines; only the final one carries the verdict.
			if len(line) < 4 || line[3] != '-' {
				return line, nil
			}
		}
	}

	exchange := func(cmd string) (string, error) {
		if cmd != "" {
			if _, err := fmt.Fprintf(conn, "%s\r\n", cmd); err != nil {
				return "", err
			}
		}
		return readReply()
	}

	steps := []string{
		"",                         // banner
		"EHLO integration.test",    //
		"MAIL FROM:<" + from + ">", //
		"RCPT TO:<" + to + ">",     //
		"DATA",                     //
	}
	for _, step := range steps {
		reply, err := exchange(step)
		if err != nil {
			return "", err
		}
		if reply == "" || reply[0] != '2' && reply[0] != '3' {
			return reply, fmt.Errorf("unexpected reply to %q: %s", step, reply)
		}
	}

	if _, err := conn.Write([]byte(message + "\r\n.\r\n")); err != nil {
		return "", err
	}
	final, err := readReply()
	if err != nil {
		return "", err
	}

	if _, err := exchange("QUIT"); err != nil {
		return final, nil // the verdict is what matters
	}
	return final, nil
}

func performDNSQuery(port int, domain string) error {
	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion(domain, dns.TypeA)

	_, _, err := c.Exchange(m, fmt.Sprintf("127.0.0.1:%d", port))
	return err
}

func TestIntegration_SMTPInteraction(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	hook, err := registerHook(ts.cfg.Server.HTTP.Port, ts.cfg.Server.API.AuthToken)
	if err != nil {
		t.Fatalf("failed to register hook: %v", err)
	}

	// The registration advertises a mail address, since the listener is up.
	wantAddr := hook.ID + "@" + ts.cfg.Server.Domain
	if hook.SMTP != wantAddr {
		t.Fatalf("hook.SMTP = %q, want %q", hook.SMTP, wantAddr)
	}

	message := "From: signup@vendor.test\r\n" +
		"Subject: Your verification code\r\n" +
		"\r\n" +
		"Your code is 123456.\r\n"

	reply, err := deliverMail(ts.cfg.Server.SMTP.Port, "signup@vendor.test", hook.SMTP, message)
	if err != nil {
		t.Fatalf("failed to deliver mail: %v (reply %q)", err, reply)
	}
	if !strings.HasPrefix(reply, "250") {
		t.Fatalf("final reply = %q, want a 250", reply)
	}

	// Give the capture a moment to land.
	time.Sleep(100 * time.Millisecond)

	interactions, err := pollHook(ts.cfg.Server.HTTP.Port, ts.cfg.Server.API.AuthToken, hook.ID)
	if err != nil {
		t.Fatalf("failed to poll hook: %v", err)
	}
	if len(interactions) != 1 {
		t.Fatalf("got %d interactions, want 1", len(interactions))
	}

	got := interactions[0]
	if got.Type != "smtp" {
		t.Errorf("type = %q, want %q", got.Type, "smtp")
	}
	if subject, _ := got.Data["subject"].(string); subject != "Your verification code" {
		t.Errorf("subject = %q, want %q", subject, "Your verification code")
	}
	if body, _ := got.Data["body"].(string); !strings.Contains(body, "Your code is 123456.") {
		t.Errorf("body = %q, want it to contain the message text", body)
	}
	if mailFrom, _ := got.Data["mail_from"].(string); mailFrom != "signup@vendor.test" {
		t.Errorf("mail_from = %q, want %q", mailFrom, "signup@vendor.test")
	}
}

func TestIntegration_SMTPSubAddressTag(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	hook, err := registerHook(ts.cfg.Server.HTTP.Port, ts.cfg.Server.API.AuthToken)
	if err != nil {
		t.Fatalf("failed to register hook: %v", err)
	}

	// One registration, unlimited labelled aliases: the tag shows which site
	// leaked the address.
	alias := hook.ID + "+carrefour@" + ts.cfg.Server.Domain
	if _, err := deliverMail(ts.cfg.Server.SMTP.Port, "news@vendor.test", alias,
		"Subject: offers\r\n\r\nbuy things\r\n"); err != nil {
		t.Fatalf("failed to deliver mail: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	interactions, err := pollHook(ts.cfg.Server.HTTP.Port, ts.cfg.Server.API.AuthToken, hook.ID)
	if err != nil {
		t.Fatalf("failed to poll hook: %v", err)
	}
	if len(interactions) != 1 {
		t.Fatalf("got %d interactions, want 1", len(interactions))
	}
	if tag, _ := interactions[0].Data["tag"].(string); tag != "carrefour" {
		t.Errorf("tag = %q, want %q", tag, "carrefour")
	}
}

func TestIntegration_SMTPRefusesRelay(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	// The one rejection that matters: it is what separates a capture server
	// from an open relay.
	reply, err := deliverMail(ts.cfg.Server.SMTP.Port, "spammer@vendor.test",
		"victim@example.org", "Subject: spam\r\n\r\nbody\r\n")
	if err == nil {
		t.Fatalf("expected the relay attempt to be refused, got reply %q", reply)
	}
	if !strings.HasPrefix(reply, "550") {
		t.Errorf("reply = %q, want a 550", reply)
	}
}

func TestIntegration_SMTPDisabledOmitsAddress(t *testing.T) {
	idGenerator := sequentialIDs()
	cfg := defaultTestConfig(filepath.Join(t.TempDir(), "longlived.db"))
	cfg.Server.SMTP.Enabled = false

	ts := startServer(t, cfg, idGenerator)
	defer ts.cleanup()

	hook, err := registerHook(ts.cfg.Server.HTTP.Port, ts.cfg.Server.API.AuthToken)
	if err != nil {
		t.Fatalf("failed to register hook: %v", err)
	}
	if hook.SMTP != "" {
		t.Errorf("hook.SMTP = %q, want it empty when no listener is running", hook.SMTP)
	}
}
