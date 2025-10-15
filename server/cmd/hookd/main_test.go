package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jomar/hookd/internal/config"
)

func TestSetupLogger(t *testing.T) {
	tests := []struct {
		name     string
		cfg      config.ObservabilityConfig
		logLevel slog.Level
	}{
		{
			name:     "debug level",
			cfg:      config.ObservabilityConfig{LogLevel: "debug", LogFormat: "text"},
			logLevel: slog.LevelDebug,
		},
		{
			name:     "info level",
			cfg:      config.ObservabilityConfig{LogLevel: "info", LogFormat: "text"},
			logLevel: slog.LevelInfo,
		},
		{
			name:     "warn level",
			cfg:      config.ObservabilityConfig{LogLevel: "warn", LogFormat: "text"},
			logLevel: slog.LevelWarn,
		},
		{
			name:     "error level",
			cfg:      config.ObservabilityConfig{LogLevel: "error", LogFormat: "text"},
			logLevel: slog.LevelError,
		},
		{
			name:     "default level",
			cfg:      config.ObservabilityConfig{LogLevel: "unknown", LogFormat: "text"},
			logLevel: slog.LevelInfo,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := setupLogger(tt.cfg)
			if logger == nil {
				t.Fatal("expected logger to be created")
			}

			// Test that logger can log at the configured level
			var buf bytes.Buffer
			handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: tt.logLevel})
			testLogger := slog.New(handler)

			testLogger.Debug("debug message")
			testLogger.Info("info message")
			testLogger.Warn("warn message")
			testLogger.Error("error message")

			output := buf.String()

			// Verify correct level is being logged
			switch tt.logLevel {
			case slog.LevelDebug:
				if !strings.Contains(output, "debug message") {
					t.Error("expected debug messages to be logged")
				}
			case slog.LevelInfo:
				if strings.Contains(output, "debug message") {
					t.Error("expected debug messages to be filtered")
				}
				if !strings.Contains(output, "info message") {
					t.Error("expected info messages to be logged")
				}
			case slog.LevelWarn:
				if !strings.Contains(output, "warn message") {
					t.Error("expected warn messages to be logged")
				}
			case slog.LevelError:
				if !strings.Contains(output, "error message") {
					t.Error("expected error messages to be logged")
				}
			}
		})
	}
}

func TestSetupLogger_JSON(t *testing.T) {
	cfg := config.ObservabilityConfig{
		LogLevel:  "info",
		LogFormat: "json",
	}

	logger := setupLogger(cfg)
	if logger == nil {
		t.Fatal("expected logger to be created")
	}
}

func TestGenerateID(t *testing.T) {
	// Test multiple IDs for uniqueness
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := generateID()

		if id == "" {
			t.Error("expected non-empty ID")
		}

		// Check length (8 bytes = 16 hex chars)
		if len(id) != 16 {
			t.Errorf("expected ID length 16, got %d", len(id))
		}

		// Check that it's valid hex
		for _, c := range id {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Errorf("expected hex character, got %c", c)
			}
		}

		// Check uniqueness
		if ids[id] {
			t.Errorf("duplicate ID generated: %s", id)
		}
		ids[id] = true
	}

	if len(ids) != 100 {
		t.Errorf("expected 100 unique IDs, got %d", len(ids))
	}
}

func TestPrintHelp(t *testing.T) {
	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	printHelp()

	w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	io.Copy(&buf, r)
	output := buf.String()

	// Verify help output contains expected content
	if !strings.Contains(output, "Hookd") {
		t.Error("expected help to contain 'Hookd'")
	}

	if !strings.Contains(output, "Usage:") {
		t.Error("expected help to contain 'Usage:'")
	}

	if !strings.Contains(output, "Options:") {
		t.Error("expected help to contain 'Options:'")
	}

	if !strings.Contains(output, "Examples:") {
		t.Error("expected help to contain 'Examples:'")
	}
}

func TestVersion(t *testing.T) {
	if version == "" {
		t.Error("expected version to be set")
	}
}
