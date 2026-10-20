package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/storage"
	"github.com/libdns/libdns"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

func TestCLIInformationalAndConfigErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		code int
		text string
	}{
		{"version", []string{"--version"}, 0, "hookd version " + version},
		{"help", []string{"--help"}, 0, "Usage:"},
		{"missing config", []string{"--config", filepath.Join(t.TempDir(), "missing.yaml")}, 1, "Error loading configuration:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldArgs, oldFlags, oldOut, oldErr := os.Args, pflag.CommandLine, os.Stdout, os.Stderr
			out, err := os.CreateTemp(t.TempDir(), "cli-output")
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			defer func() {
				os.Args = oldArgs
				pflag.CommandLine = oldFlags
				os.Stdout = oldOut
				os.Stderr = oldErr
				viper.Reset()
			}()
			viper.Reset()
			pflag.CommandLine = pflag.NewFlagSet("hookd", pflag.ContinueOnError)
			os.Args = append([]string{"hookd"}, tc.args...)
			os.Stdout, os.Stderr = out, out
			code := runCLI()
			if code != tc.code {
				t.Fatalf("exit code=%d, want %d", code, tc.code)
			}
			data, err := os.ReadFile(out.Name())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), tc.text) {
				t.Fatalf("missing expected output %q", tc.text)
			}
		})
	}
}

func startupConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Server.Domain = "startup.test"
	cfg.Server.PublicIPs = []string{"127.0.0.1"}
	cfg.Server.DNS.Enabled = false
	cfg.Server.DNS.BindAddress = "127.0.0.1"
	cfg.Server.DNS.Port = 0
	cfg.Server.SMTP.BindAddress = "127.0.0.1"
	cfg.Server.SMTP.Port = 0
	cfg.Server.API.AuthToken = "test-startup-token"
	cfg.Server.HTTP.Port = -1 // deterministic local listener failure, no port collision
	cfg.LongLived.DBPath = filepath.Join(t.TempDir(), "hooks.db")
	cfg.Observability.LogLevel = "error"
	return cfg
}

func TestRunStopsAfterListenerFailure(t *testing.T) {
	cfg := startupConfig(t)
	cfg.Server.DNS.Enabled = true
	cfg.Server.SMTP.Enabled = true
	done := make(chan error, 1)
	go func() { done <- run(cfg) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not stop after listener failure")
	}
	// The deferred close must allow a new manager to reopen the same store.
	m, err := openStore(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := m.CreateLongLivedHook(cfg.Server.Domain, storage.CreateOptions{TTL: 48 * time.Hour}, 1); err != nil {
		t.Fatal(err)
	}
}

func TestRunReportsStartupFailures(t *testing.T) {
	t.Run("storage", func(t *testing.T) {
		cfg := startupConfig(t)
		parent := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(parent, []byte("not a directory"), 0600); err != nil {
			t.Fatal(err)
		}
		cfg.LongLived.DBPath = filepath.Join(parent, "hooks.db")
		if err := run(cfg); err == nil || !strings.Contains(err.Error(), "opening long-lived store") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("smtp", func(t *testing.T) {
		cfg := startupConfig(t)
		cfg.Server.SMTP.Enabled = true
		cfg.Server.SMTP.Port = -1
		if err := run(cfg); err == nil || !strings.Contains(err.Error(), "failed to create smtp server") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestOpenStorageLifetimeAndCapacity(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "persistent"}[enabled], func(t *testing.T) {
			cfg := startupConfig(t)
			cfg.LongLived.Enabled = enabled
			cfg.Eviction.MaxPerHook = 1
			m, err := openStore(cfg, logger)
			if err != nil {
				t.Fatal(err)
			}
			hook := m.CreateHook(cfg.Server.Domain, storage.CreateOptions{TTL: cfg.Eviction.HookTTL})
			m.AddInteraction(hook.ID, storage.DNSInteraction("one", "127.0.0.1", "q", "A"))
			m.AddInteraction(hook.ID, storage.DNSInteraction("two", "127.0.0.1", "q", "A"))
			read, err := m.ReadInteractions(hook.ID, 0)
			if err != nil || len(read.Interactions) != 1 || read.Interactions[0].ID != "two" {
				t.Fatalf("per-hook cap not applied: %+v, %v", read, err)
			}
			durable, err := m.CreateLongLivedHook(cfg.Server.Domain, storage.CreateOptions{TTL: 48 * time.Hour}, 1)
			if !enabled && err == nil {
				t.Fatal("disabled persistence accepted a durable hook")
			}
			if enabled && err != nil {
				t.Fatal(err)
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := openStore(cfg, logger)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if reopened.Has(hook.ID) {
				t.Fatal("ephemeral hook survived reopen")
			}
			if enabled && !reopened.Has(durable.ID) {
				t.Fatal("durable hook lost on reopen")
			}
		})
	}
}

func TestOpenRedisStorage(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("unreachable", func(t *testing.T) {
		cfg := startupConfig(t)
		cfg.Storage.Backend = config.StorageBackendRedis
		cfg.Storage.Redis.Addr = "127.0.0.1:1"
		cfg.Storage.Redis.DialTimeout = 100 * time.Millisecond
		if _, err := openStore(cfg, logger); err == nil || !strings.Contains(err.Error(), "opening redis store") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("shared across reopen", func(t *testing.T) {
		addr := os.Getenv("HOOKD_TEST_REDIS_ADDR")
		if addr == "" {
			t.Skip("HOOKD_TEST_REDIS_ADDR is not set")
		}
		cfg := startupConfig(t)
		cfg.Storage.Backend = config.StorageBackendRedis
		cfg.Storage.Redis.Addr = addr
		cfg.Storage.Redis.KeyPrefix = "hookdtest:startup:" + generateID() + ":"
		cfg.Eviction.MaxPerHook = 1
		t.Cleanup(func() {
			ctx := context.Background()
			client := redis.NewClient(&redis.Options{Addr: addr})
			defer client.Close()
			if keys, _ := client.Keys(ctx, cfg.Storage.Redis.KeyPrefix+"*").Result(); len(keys) > 0 {
				client.Del(ctx, keys...)
			}
		})

		m, err := openStore(cfg, logger)
		if err != nil {
			t.Fatal(err)
		}
		hook := m.CreateHook(cfg.Server.Domain, storage.CreateOptions{TTL: cfg.Eviction.HookTTL})
		m.AddInteraction(hook.ID, storage.DNSInteraction("one", "127.0.0.1", "q", "A"))
		m.AddInteraction(hook.ID, storage.DNSInteraction("two", "127.0.0.1", "q", "A"))
		durable, err := m.CreateLongLivedHook(cfg.Server.Domain, storage.CreateOptions{TTL: 48 * time.Hour}, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}

		// Unlike the local backend, a restarted instance sees every hook.
		reopened, err := openStore(cfg, logger)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		read, err := reopened.ReadInteractions(hook.ID, 0)
		if err != nil || len(read.Interactions) != 1 || read.Interactions[0].ID != "two" {
			t.Fatalf("per-hook cap not applied or hook lost: %+v, %v", read, err)
		}
		if !reopened.Has(durable.ID) {
			t.Fatal("durable hook lost on reopen")
		}
		reopened.EvictExpiredHooks(time.Now().Add(100 * time.Hour))
		if reopened.Has(hook.ID) || reopened.Has(durable.ID) {
			t.Fatal("cleanup left hooks behind")
		}

		// ACME state goes to the same server, under the same prefix: what one
		// instance writes, another reads.
		a, err := openBackend(cfg, logger, generateID)
		if err != nil {
			t.Fatal(err)
		}
		defer a.store.Close()
		b, err := openBackend(cfg, logger, generateID)
		if err != nil {
			t.Fatal(err)
		}
		defer b.store.Close()
		if a.certs == nil {
			t.Fatal("expected shared certificate storage")
		}
		ctx := context.Background()
		if err := a.certs.Store(ctx, "k", []byte("v")); err != nil {
			t.Fatal(err)
		}
		if got, err := b.certs.Load(ctx, "k"); err != nil || string(got) != "v" {
			t.Fatalf("expected the certificate on the other instance, got %q, %v", got, err)
		}
		if _, err := a.acme.AppendRecords(ctx, "startup.test.", []libdns.Record{libdns.RR{Type: "TXT", Name: "_acme-challenge", Data: "x"}}); err != nil {
			t.Fatal(err)
		}
		if recs, err := b.acme.GetRecords(ctx, "startup.test."); err != nil || len(recs) != 1 {
			t.Fatalf("expected the record on the other instance, got %v, %v", recs, err)
		}
	})
}

func TestOpenBackendLocal(t *testing.T) {
	b, err := openBackend(startupConfig(t), slog.New(slog.NewTextHandler(io.Discard, nil)), generateID)
	if err != nil {
		t.Fatal(err)
	}
	defer b.store.Close()
	if b.acme == nil || b.certs != nil {
		t.Fatalf("the local backend keeps ACME state in memory and on disk, got %v, %v", b.acme, b.certs)
	}
}

// openStore opens the configured backend for tests that only need its store.
func openStore(cfg *config.Config, logger *slog.Logger) (storage.Store, error) {
	b, err := openBackend(cfg, logger, generateID)
	if err != nil {
		return nil, err
	}
	return b.store, nil
}
