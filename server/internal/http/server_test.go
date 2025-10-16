package http

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jomar/hookd/internal/acme"
	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/eviction"
	"github.com/jomar/hookd/internal/storage"
)

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

	server := NewServer(cfg, manager, evictor, acmeProvider, logger, idGen)

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

	server := NewServer(cfg, manager, evictor, acmeProvider, logger, idGen)

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

	server := NewServer(cfg, manager, evictor, acmeProvider, logger, idGen)

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

	server := NewServer(cfg, manager, evictor, acmeProvider, logger, idGen)

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

	server := NewServer(cfg, manager, evictor, acmeProvider, logger, idGen)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Start(ctx)
	time.Sleep(200 * time.Millisecond)

	// Test that middleware is applied (logging, recovery)
	t.Run("wildcard capture", func(t *testing.T) {
		hook := manager.CreateHook("example.com")

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
		hook := manager.CreateHook("example.com")

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

	server := NewServer(cfg, manager, evictor, acmeProvider, logger, idGen)

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
