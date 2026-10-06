package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/storage"
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
	cfg.Server.PublicIP = "127.0.0.1"
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
	m, err := openStorage(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), generateID)
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
			m, err := openStorage(cfg, logger, generateID)
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
			reopened, err := openStorage(cfg, logger, generateID)
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
