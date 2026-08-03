package http

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jomar/hookd/internal/acme"
	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/eviction"
	"github.com/jomar/hookd/internal/storage"
)

func TestDefaultACMEResolvers(t *testing.T) {
	resolvers := defaultACMEResolvers()
	if len(resolvers) == 0 {
		t.Fatal("expected default resolvers to be non-empty")
	}
	// Every default resolver must carry an explicit :53 port so CertMagic can
	// dial it directly.
	for _, r := range resolvers {
		if !strings.HasSuffix(r, ":53") {
			t.Errorf("expected resolver %q to specify port 53", r)
		}
	}
}

func TestSuppressedTLSWriter(t *testing.T) {
	// Capture logged errors by counting records emitted to a recording handler.
	rec := &recordingHandler{}
	w := &suppressedTLSWriter{logger: slog.New(rec)}

	t.Run("suppresses TLS handshake noise", func(t *testing.T) {
		rec.count = 0
		for _, msg := range []string{
			"http: TLS handshake error from 1.2.3.4: EOF",
			"http: TLS handshake error: no certificate available for \"x.com\"",
		} {
			n, err := w.Write([]byte(msg))
			if err != nil || n != len(msg) {
				t.Errorf("expected full write with no error, got n=%d err=%v", n, err)
			}
		}
		if rec.count != 0 {
			t.Errorf("expected TLS handshake errors suppressed, but %d were logged", rec.count)
		}
	})

	t.Run("logs other errors", func(t *testing.T) {
		rec.count = 0
		msg := "http: some other server error"
		n, err := w.Write([]byte(msg))
		if err != nil || n != len(msg) {
			t.Errorf("expected full write with no error, got n=%d err=%v", n, err)
		}
		if rec.count != 1 {
			t.Errorf("expected non-TLS error to be logged once, got %d", rec.count)
		}
	})
}

// recordingHandler is a minimal slog.Handler that counts emitted records.
type recordingHandler struct{ count int }

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(context.Context, slog.Record) error { h.count++; return nil }
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler        { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler             { return h }

func TestNewServer(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	evictorCfg := config.EvictionConfig{
		CleanupInterval: 60,
		InteractionTTL:  3600,
		MaxPerHook:      100,
		MaxMemoryMB:     100,
	}
	evictor := eviction.NewEvictor(manager, evictorCfg, slog.Default())
	acmeProvider := acme.NewProvider(slog.Default())
	logger := slog.Default()

	cfg := config.ServerConfig{
		Domain: "example.com",
		DNS: config.DNSConfig{
			Enabled: true,
			Port:    53,
		},
		HTTP: config.HTTPConfig{
			Port: 8080,
		},
		HTTPS: config.HTTPSConfig{
			Enabled:  false,
			Port:     8443,
			AutoCert: false,
		},
		API: config.APIConfig{
			AuthToken: "test-token",
		},
	}

	server := NewServer(cfg, config.LongLivedConfig{}, config.ObservabilityConfig{MetricsEnabled: true}, manager, evictor, acmeProvider, logger, idGen)

	if server == nil {
		t.Fatal("expected server to be created")
	}

	if server.config.Domain != "example.com" {
		t.Errorf("expected domain example.com, got %s", server.config.Domain)
	}
}

func TestServer_StartHTTPOnly(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	evictorCfg := config.EvictionConfig{
		CleanupInterval: 60,
		InteractionTTL:  3600,
		MaxPerHook:      100,
		MaxMemoryMB:     100,
	}
	evictor := eviction.NewEvictor(manager, evictorCfg, slog.Default())
	acmeProvider := acme.NewProvider(slog.Default())
	logger := slog.Default()

	cfg := config.ServerConfig{
		Domain: "example.com",
		DNS: config.DNSConfig{
			Enabled: false,
		},
		HTTP: config.HTTPConfig{
			Port: 0, // Use random port
		},
		HTTPS: config.HTTPSConfig{
			Enabled: false,
		},
		API: config.APIConfig{
			AuthToken: "test-token",
		},
	}

	server := NewServer(cfg, config.LongLivedConfig{}, config.ObservabilityConfig{MetricsEnabled: true}, manager, evictor, acmeProvider, logger, idGen)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start server in background
	errChan := make(chan error, 1)
	go func() {
		errChan <- server.Start(ctx)
	}()

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	// Test that server is responding
	// Note: We can't easily test the exact port since we're using port 0
	// but we can cancel and check clean shutdown

	// Cancel context to stop server
	cancel()

	// Wait for server to stop
	select {
	case err := <-errChan:
		if err != nil && err != context.Canceled {
			t.Errorf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop in time")
	}
}

func TestServer_Endpoints(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	evictorCfg := config.EvictionConfig{
		CleanupInterval: 60,
		InteractionTTL:  3600,
		MaxPerHook:      100,
		MaxMemoryMB:     100,
	}
	evictor := eviction.NewEvictor(manager, evictorCfg, slog.Default())
	acmeProvider := acme.NewProvider(slog.Default())
	logger := slog.Default()

	cfg := config.ServerConfig{
		Domain: "example.com",
		DNS: config.DNSConfig{
			Enabled: false,
		},
		HTTP: config.HTTPConfig{
			Port: 18888, // Use high port to avoid conflicts
		},
		HTTPS: config.HTTPSConfig{
			Enabled: false,
		},
		API: config.APIConfig{
			AuthToken: "test-token",
		},
	}

	server := NewServer(cfg, config.LongLivedConfig{}, config.ObservabilityConfig{MetricsEnabled: true}, manager, evictor, acmeProvider, logger, idGen)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start server in background
	go server.Start(ctx)

	// Give server time to start
	time.Sleep(200 * time.Millisecond)

	// Test metrics endpoint (no auth required)
	t.Run("metrics endpoint", func(t *testing.T) {
		resp, err := http.Get("http://localhost:18888/metrics")
		if err != nil {
			t.Fatalf("failed to request metrics: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}
	})

	// Test register endpoint without auth
	t.Run("register without auth", func(t *testing.T) {
		resp, err := http.Post("http://localhost:18888/register", "application/json", nil)
		if err != nil {
			t.Fatalf("failed to request register: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected status 401, got %d", resp.StatusCode)
		}
	})

	// Test register endpoint with auth
	t.Run("register with auth", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "http://localhost:18888/register", nil)
		req.Header.Set("X-API-Key", "test-token")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("failed to request register: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("expected status 200, got %d: %s", resp.StatusCode, body)
		}
	})

	// Stop server
	cancel()
	time.Sleep(100 * time.Millisecond)
}

func TestServer_StartHTTPSManualDisabled(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	evictorCfg := config.EvictionConfig{
		CleanupInterval: 60,
		InteractionTTL:  3600,
		MaxPerHook:      100,
		MaxMemoryMB:     100,
	}
	evictor := eviction.NewEvictor(manager, evictorCfg, slog.Default())
	acmeProvider := acme.NewProvider(slog.Default())
	logger := slog.Default()

	cfg := config.ServerConfig{
		Domain: "example.com",
		DNS: config.DNSConfig{
			Enabled: false,
		},
		HTTP: config.HTTPConfig{
			Port: 0, // Random port
		},
		HTTPS: config.HTTPSConfig{
			Enabled:  true,
			Port:     0,
			AutoCert: false, // Manual TLS - should trigger warning
			CacheDir: "/tmp/hookd-test",
		},
		API: config.APIConfig{
			AuthToken: "test-token",
		},
	}

	server := NewServer(cfg, config.LongLivedConfig{}, config.ObservabilityConfig{MetricsEnabled: true}, manager, evictor, acmeProvider, logger, idGen)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start server in background
	errChan := make(chan error, 1)
	go func() {
		errChan <- server.Start(ctx)
	}()

	// Give server time to start and log warning
	time.Sleep(100 * time.Millisecond)

	// Cancel context to stop server
	cancel()

	// Wait for server to stop
	select {
	case err := <-errChan:
		if err != nil && err != context.Canceled {
			t.Errorf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop in time")
	}
}

func TestServer_MiddlewareChain(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	evictorCfg := config.EvictionConfig{
		CleanupInterval: 60,
		InteractionTTL:  3600,
		MaxPerHook:      100,
		MaxMemoryMB:     100,
	}
	evictor := eviction.NewEvictor(manager, evictorCfg, slog.Default())
	acmeProvider := acme.NewProvider(slog.Default())
	logger := slog.Default()

	cfg := config.ServerConfig{
		Domain: "example.com",
		DNS: config.DNSConfig{
			Enabled: false,
		},
		HTTP: config.HTTPConfig{
			Port: 18889, // Different port
		},
		HTTPS: config.HTTPSConfig{
			Enabled: false,
		},
		API: config.APIConfig{
			AuthToken: "test-token",
		},
	}

	server := NewServer(cfg, config.LongLivedConfig{}, config.ObservabilityConfig{MetricsEnabled: true}, manager, evictor, acmeProvider, logger, idGen)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Start(ctx)
	time.Sleep(200 * time.Millisecond)

	// Test that middleware is applied (logging, recovery)
	t.Run("wildcard capture", func(t *testing.T) {
		hook := manager.CreateHook("example.com", storage.CreateOptions{})

		req, _ := http.NewRequest(http.MethodGet, "http://localhost:18889/anything", nil)
		req.Host = hook.ID + ".example.com"

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("failed to request: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}
	})

	// Test poll endpoint with different paths
	t.Run("poll endpoint variations", func(t *testing.T) {
		hook := manager.CreateHook("example.com", storage.CreateOptions{})

		// Valid poll
		req, _ := http.NewRequest(http.MethodGet, "http://localhost:18889/poll/"+hook.ID, nil)
		req.Header.Set("X-API-Key", "test-token")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("failed to request poll: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}
	})

	cancel()
	time.Sleep(100 * time.Millisecond)
}

func TestServer_ContextCancellation(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	evictorCfg := config.EvictionConfig{
		CleanupInterval: 60,
		InteractionTTL:  3600,
		MaxPerHook:      100,
		MaxMemoryMB:     100,
	}
	evictor := eviction.NewEvictor(manager, evictorCfg, slog.Default())
	acmeProvider := acme.NewProvider(slog.Default())
	logger := slog.Default()

	cfg := config.ServerConfig{
		Domain: "example.com",
		DNS: config.DNSConfig{
			Enabled: false,
		},
		HTTP: config.HTTPConfig{
			Port: 0,
		},
		HTTPS: config.HTTPSConfig{
			Enabled: false,
		},
		API: config.APIConfig{
			AuthToken: "test-token",
		},
	}

	server := NewServer(cfg, config.LongLivedConfig{}, config.ObservabilityConfig{MetricsEnabled: true}, manager, evictor, acmeProvider, logger, idGen)

	// Test that context cancellation stops the server gracefully
	ctx, cancel := context.WithCancel(context.Background())

	errChan := make(chan error, 1)
	go func() {
		errChan <- server.Start(ctx)
	}()

	time.Sleep(100 * time.Millisecond)

	// Cancel immediately
	cancel()

	select {
	case err := <-errChan:
		if err != nil && err != context.Canceled {
			t.Errorf("expected nil or context.Canceled error, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop after context cancellation")
	}
}

func TestNewPublicServer_Deadlines(t *testing.T) {
	srv := newPublicServer(":8080", http.NewServeMux(), slog.Default())

	// A zero value means "no deadline", so every bound must be set.
	deadlines := map[string]time.Duration{
		"ReadHeaderTimeout": srv.ReadHeaderTimeout,
		"ReadTimeout":       srv.ReadTimeout,
		"WriteTimeout":      srv.WriteTimeout,
		"IdleTimeout":       srv.IdleTimeout,
	}
	for name, d := range deadlines {
		if d <= 0 {
			t.Errorf("expected %s to be set, got %v", name, d)
		}
	}

	if srv.MaxHeaderBytes <= 0 {
		t.Errorf("expected MaxHeaderBytes to be set, got %d", srv.MaxHeaderBytes)
	}

	// The read budget must cover a multi-megabyte capture body.
	if srv.ReadTimeout < srv.ReadHeaderTimeout {
		t.Errorf("expected ReadTimeout (%v) >= ReadHeaderTimeout (%v)", srv.ReadTimeout, srv.ReadHeaderTimeout)
	}
}

func TestRouteByHost(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	capture := NewCaptureHandler(manager, "example.com", slog.Default(), idGen, 0)

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // marks "the API answered"
	})
	apiMux.Handle("/", capture)

	handler := routeByHost(apiMux, capture)

	t.Run("api path on a hook subdomain is captured", func(t *testing.T) {
		hook := manager.CreateHook("example.com", storage.CreateOptions{})

		req := httptest.NewRequest(http.MethodPost, "/register", nil)
		req.Host = hook.ID + ".example.com"

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code == http.StatusTeapot {
			t.Fatal("expected the capture handler to win over the API route on a hook host")
		}
		if n := len(manager.PollInteractions(hook.ID)); n != 1 {
			t.Errorf("expected the callback to be recorded, got %d interactions", n)
		}
	})

	t.Run("api path on a mixed-case hook subdomain is captured", func(t *testing.T) {
		hook := manager.CreateHook("example.com", storage.CreateOptions{})

		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.Host = strings.ToUpper(hook.ID) + ".ExAmPlE.CoM"

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if n := len(manager.PollInteractions(hook.ID)); n != 1 {
			t.Errorf("expected the mixed-case callback to be recorded, got %d interactions", n)
		}
	})

	t.Run("api reachable on the apex domain", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/register", nil)
		req.Host = "example.com"

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusTeapot {
			t.Errorf("expected the API to answer on the apex domain, got status %d", w.Code)
		}
	})

	t.Run("api reachable on an arbitrary host", func(t *testing.T) {
		// Deployments address the API by IP or proxy hostname.
		for _, host := range []string{"127.0.0.1:8080", "hookd.internal"} {
			req := httptest.NewRequest(http.MethodPost, "/register", nil)
			req.Host = host

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusTeapot {
				t.Errorf("expected the API to answer on host %q, got status %d", host, w.Code)
			}
		}
	})
}

func TestServer_MetricsDisabled(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	evictor := eviction.NewEvictor(manager, config.EvictionConfig{
		CleanupInterval: 60,
		InteractionTTL:  3600,
		MaxPerHook:      100,
		MaxMemoryMB:     100,
	}, slog.Default())

	cfg := config.ServerConfig{
		Domain: "example.com",
		HTTP:   config.HTTPConfig{Port: 18890},
		API:    config.APIConfig{AuthToken: "test-token"},
	}

	server := NewServer(cfg, config.LongLivedConfig{}, config.ObservabilityConfig{MetricsEnabled: false},
		manager, evictor, acme.NewProvider(slog.Default()), slog.Default(), idGen)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Start(ctx)
	time.Sleep(200 * time.Millisecond)

	resp, err := http.Get("http://localhost:18890/metrics")
	if err != nil {
		t.Fatalf("failed to request metrics: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	// The request falls through to the capture catch-all, disclosing nothing.
	for _, leak := range []string{"hooks", "interactions", "evictions", "memory"} {
		if strings.Contains(string(body), leak) {
			t.Errorf("expected no metrics disclosed, found %q in %q", leak, body)
		}
	}

	cancel()
	time.Sleep(100 * time.Millisecond)
}
