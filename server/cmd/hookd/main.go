package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/jomar/hookd/internal/acme"
	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/dns"
	"github.com/jomar/hookd/internal/eviction"
	"github.com/jomar/hookd/internal/http"
	"github.com/jomar/hookd/internal/storage"
)

const version = "0.1.0"

func main() {
	// Register CLI flags
	config.RegisterFlags()
	pflag.Parse()

	// Bind flags to viper
	if err := viper.BindPFlags(pflag.CommandLine); err != nil {
		fmt.Fprintf(os.Stderr, "Error binding flags: %v\n", err)
		os.Exit(1)
	}

	// Handle version flag
	if viper.GetBool("version") {
		fmt.Printf("hookd version %s\n", version)
		os.Exit(0)
	}

	// Handle help flag
	if viper.GetBool("help") {
		printHelp()
		os.Exit(0)
	}

	// Load configuration
	configPath := viper.GetString("config")
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading configuration: %v\n", err)
		os.Exit(1)
	}

	// Setup logger
	logger := setupLogger(cfg.Observability)

	// Ensure auth token exists
	token, generated := cfg.EnsureAuthToken()
	if generated {
		logger.Info("auth token generated", "token", token)
	} else {
		logger.Info("using configured auth token")
	}

	// Display startup banner
	logger.Info("hookd starting",
		"version", version,
		"domain", cfg.Server.Domain,
		"dns_enabled", cfg.Server.DNS.Enabled,
		"https_enabled", cfg.Server.HTTPS.Enabled)

	// Create ID generator
	idGenerator := func() string {
		return generateID()
	}

	// Create storage manager
	storageManager := storage.NewMemoryManager(idGenerator)

	// Create ACME provider for DNS-01 challenges
	acmeProvider := acme.NewProvider(logger)

	// Create evictor
	evictor := eviction.NewEvictor(storageManager, cfg.Eviction, logger)

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start eviction system
	go evictor.Start(ctx)

	// Start DNS server if enabled
	if cfg.Server.DNS.Enabled {
		dnsServer, err := dns.NewServer(
			cfg.Server.Domain,
			cfg.Server.DNS.Port,
			storageManager,
			acmeProvider,
			logger,
			idGenerator,
		)
		if err != nil {
			logger.Error("failed to create dns server", "error", err)
			os.Exit(1)
		}

		go func() {
			if err := dnsServer.Start(ctx); err != nil {
				logger.Error("dns server error", "error", err)
				cancel()
			}
		}()
	}

	// Start HTTP/HTTPS server
	httpServer := http.NewServer(
		cfg.Server,
		storageManager,
		evictor,
		acmeProvider,
		logger,
		idGenerator,
	)

	go func() {
		if err := httpServer.Start(ctx); err != nil {
			logger.Error("http server error", "error", err)
			cancel()
		}
	}()

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	select {
	case <-sigChan:
		logger.Info("shutdown signal received")
	case <-ctx.Done():
		logger.Info("context cancelled")
	}

	// Graceful shutdown
	cancel()
	logger.Info("hookd stopped")
}

// setupLogger creates and configures a logger
func setupLogger(cfg config.ObservabilityConfig) *slog.Logger {
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: level,
	}

	var handler slog.Handler
	if cfg.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}

// generateID generates a random alphanumeric ID
func generateID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("failed to generate ID: %v", err))
	}
	return hex.EncodeToString(b)
}

// printHelp prints usage information
func printHelp() {
	fmt.Println("Hookd - High-performance DNS/HTTP interaction server")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  hookd [options]")
	fmt.Println()
	fmt.Println("Options:")
	pflag.PrintDefaults()
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  # Start with config file")
	fmt.Println("  hookd --config /etc/hookd/config.yaml")
	fmt.Println()
	fmt.Println("  # Override token")
	fmt.Println("  hookd --config config.yaml --token my-secret-token")
	fmt.Println()
	fmt.Println("  # Override domain and ports")
	fmt.Println("  hookd --config config.yaml --domain hookd.example.com --dns-port 53 --http-port 80")
}
