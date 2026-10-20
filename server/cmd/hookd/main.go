package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/caddyserver/certmagic"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/jomar/hookd/internal/acme"
	"github.com/jomar/hookd/internal/certstore"
	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/dns"
	"github.com/jomar/hookd/internal/eviction"
	"github.com/jomar/hookd/internal/http"
	"github.com/jomar/hookd/internal/smtp"
	"github.com/jomar/hookd/internal/storage"
)

// version is overridden at build time via -ldflags "-X main.version=..."
var version = "dev"

func main() {
	os.Exit(runCLI())
}

// runCLI returns an exit status so CLI behavior can be tested without exiting
// the process. Listener and storage failures retain their existing messages.
func runCLI() int {
	// Register CLI flags
	config.RegisterFlags()
	pflag.Parse()

	// Bind flags to viper
	if err := viper.BindPFlags(pflag.CommandLine); err != nil {
		fmt.Fprintf(os.Stderr, "Error binding flags: %v\n", err)
		return 1
	}

	// Handle version flag
	if viper.GetBool("version") {
		fmt.Printf("hookd version %s\n", version)
		return 0
	}

	// Handle help flag
	if viper.GetBool("help") {
		printHelp()
		return 0
	}

	// Load configuration
	configPath := viper.GetString("config")
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading configuration: %v\n", err)
		return 1
	}

	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// run stops on SIGINT or SIGTERM.
func run(cfg *config.Config) error {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)
	return runUntil(cfg, sigChan)
}

// runUntil owns the service lifetime until stop fires or a listener fails,
// and closes storage when startup fails.
func runUntil(cfg *config.Config, stop <-chan os.Signal) error {
	// Setup logger
	logger := setupLogger(cfg.Observability)

	// The token stays out of the structured logs, which are commonly shipped to
	// aggregators; a generated one goes to stderr once.
	token, generated := cfg.EnsureAuthToken()
	if generated {
		fmt.Fprintf(os.Stderr, "Generated API token: %s\n"+
			"Store it now — set server.api.auth_token to keep it across restarts.\n", token)
	}
	logger.Info("auth token ready", "generated", generated, "fingerprint", tokenFingerprint(token))

	// Display startup banner
	logger.Info("hookd starting",
		"version", version,
		"domain", cfg.Server.Domain,
		"dns_enabled", cfg.Server.DNS.Enabled,
		"https_enabled", cfg.Server.HTTPS.Enabled,
		"smtp_enabled", cfg.Server.SMTP.Enabled,
		"api_listener_enabled", cfg.Server.API.Listener.Enabled,
		"owner_secret_enabled", cfg.Server.API.OwnerSecret.Enabled)

	// Create ID generator
	idGenerator := avoidLabels(generateID, nameserverLabels(cfg.Server))

	opened, err := openBackend(cfg, logger, idGenerator)
	if err != nil {
		return err
	}
	storageManager := opened.store

	defer func() {
		if err := storageManager.Close(); err != nil {
			logger.Error("failed to close storage", "error", err)
		}
	}()

	// Create ACME provider for DNS-01 challenges
	acmeProvider, certStorage := opened.acme, opened.certs

	// Create evictor
	evictor := eviction.NewEvictor(storageManager, cfg.Eviction, logger)

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start eviction system
	go evictor.Start(ctx)

	// Start DNS server if enabled
	if cfg.Server.DNS.Enabled {
		var rl config.DNSRateLimitConfig
		if cfg.Server.DNS.RateLimit.Enabled {
			rl = cfg.Server.DNS.RateLimit
		}
		dnsServer, err := dns.NewServer(dns.ServerOptions{
			Domain:             cfg.Server.Domain,
			Port:               cfg.Server.DNS.Port,
			PublicIPs:          cfg.Server.PublicIPs,
			Nameservers:        nameservers(cfg.Server.DNS.Nameservers),
			SOAMinimumTTL:      cfg.Server.DNS.SOAMinimumTTL,
			MaxTCPConnections:  cfg.Server.DNS.MaxTCPConnections,
			RateLimitPerSecond: rl.ResponsesPerSecond,
			RateLimitBurst:     rl.Burst,
			BindAddress:        cfg.Server.DNS.BindAddress,
			Storage:            storageManager,
			ACMEProvider:       acmeProvider,
			Logger:             logger,
			IDGenerator:        idGenerator,
		})
		if err != nil {
			return fmt.Errorf("failed to create dns server: %w", err)
		}

		go serve(ctx, cancel, logger, "dns", dnsServer.Start)
	}

	// Start SMTP server if enabled
	if cfg.Server.SMTP.Enabled {
		smtpServer, err := smtp.NewServer(smtp.ServerOptions{
			Domain:       cfg.Server.Domain,
			SMTP:         cfg.Server.SMTP,
			MaxBodyBytes: cfg.Eviction.MaxInteractionBodyBytes,
			Storage:      storageManager,
			Logger:       logger,
			IDGenerator:  idGenerator,
		})
		if err != nil {
			return fmt.Errorf("failed to create smtp server: %w", err)
		}

		go serve(ctx, cancel, logger, "smtp", smtpServer.Start)
	}

	// Start HTTP/HTTPS servers and, when enabled, the API listener
	httpServer := http.NewServer(http.ServerOptions{
		Server:        cfg.Server,
		LongLived:     cfg.LongLived,
		Observability: cfg.Observability,
		Storage:       storageManager,
		Evictor:       evictor,
		ACMEProvider:  acmeProvider,
		CertStorage:   certStorage,
		Logger:        logger,
		IDGenerator:   idGenerator,
	})

	go serve(ctx, cancel, logger, "http", httpServer.Start)

	select {
	case <-stop:
		logger.Info("shutdown signal received")
	case <-ctx.Done():
		logger.Info("context cancelled")
	}

	// Graceful shutdown
	cancel()
	logger.Info("hookd stopped")
	return nil
}

// backend is everything kept in the configured store.
type backend struct {
	store storage.Store
	acme  *acme.Provider
	// certs is nil for the local backend, which keeps certificates on disk.
	certs certmagic.Storage
}

// openBackend builds the store selected by storage.backend and the ACME state
// next to it: in Redis when shared, so any instance answers a challenge and
// only one obtains each certificate; otherwise in memory and on disk. The
// Redis client is shared by all three and closed with the store.
func openBackend(cfg *config.Config, logger *slog.Logger, idGenerator func() string) (*backend, error) {
	if cfg.Storage.Backend != config.StorageBackendRedis {
		store, err := openLocalStorage(cfg, logger, idGenerator)
		if err != nil {
			return nil, err
		}
		return &backend{store: store, acme: acme.NewProvider(logger)}, nil
	}
	rc := cfg.Storage.Redis
	client := newRedisClient(rc)
	store, err := openRedisStorage(cfg, client, logger, idGenerator)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	return &backend{
		store: store,
		acme:  acme.NewSharedProvider(logger, acme.NewRedisRecords(client, rc.KeyPrefix)),
		certs: certstore.NewRedis(client, rc.KeyPrefix),
	}, nil
}

// openLocalStorage builds the memory store and optional persistent store.
func openLocalStorage(cfg *config.Config, logger *slog.Logger, idGenerator func() string) (*storage.CompositeManager, error) {
	// Create storage manager: ephemeral hooks live in memory; when enabled,
	// long-lived hooks are persisted to SQLite so they survive restarts.
	memoryManager := storage.NewMemoryManager(idGenerator)
	memoryManager.SetMaxPerHook(cfg.Eviction.MaxPerHook)
	var err error
	var longLived *storage.SQLiteManager
	if cfg.LongLived.Enabled {
		longLived, err = storage.NewSQLiteManager(
			cfg.LongLived.DBPath,
			idGenerator,
			cfg.LongLived.MaxInteractionBodyBytes,
			logger,
		)
		if err != nil {
			return nil, fmt.Errorf("Error opening long-lived store: %w", err)
		}
		logger.Info("long-lived store enabled",
			"db_path", cfg.LongLived.DBPath,
			"max_hooks", cfg.LongLived.MaxHooks,
			"max_ttl", cfg.LongLived.MaxTTL)
	}
	return storage.NewCompositeManager(memoryManager, longLived, cfg.Eviction.HookTTL), nil
}

// newRedisClient connects to the Redis or Valkey server shared by every
// instance.
func newRedisClient(rc config.RedisConfig) *redis.Client {
	opts := &redis.Options{
		Addr:         rc.Addr,
		Username:     rc.Username,
		Password:     rc.Password,
		DialTimeout:  rc.DialTimeout,
		ReadTimeout:  rc.ReadTimeout,
		WriteTimeout: rc.WriteTimeout,
		PoolSize:     rc.PoolSize,
		// The store bounds pipelines and the drain on close with context
		// deadlines, which go-redis ignores without this.
		ContextTimeoutEnabled: true,
	}
	if rc.TLS {
		host, _, err := net.SplitHostPort(rc.Addr)
		if err != nil {
			host = rc.Addr
		}
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	}
	return redis.NewClient(opts)
}

// openRedisStorage checks the server and builds the store on client.
func openRedisStorage(cfg *config.Config, client *redis.Client, logger *slog.Logger, idGenerator func() string) (*storage.RedisManager, error) {
	rc := cfg.Storage.Redis
	ctx, cancel := context.WithTimeout(context.Background(), rc.DialTimeout+rc.ReadTimeout)
	defer cancel()
	m, err := storage.NewRedisManager(ctx, client, storage.RedisOptions{
		KeyPrefix:          rc.KeyPrefix,
		IDGenerator:        idGenerator,
		EphemeralThreshold: cfg.Eviction.HookTTL,
		LongLivedEnabled:   cfg.LongLived.Enabled,
		MaxPerHook:         cfg.Eviction.MaxPerHook,
		MaxBodyBytes:       cfg.LongLived.MaxInteractionBodyBytes,
		HasCacheSize:       rc.HasCacheSize,
		HasCacheTTL:        rc.HasCacheTTL,
		Logger:             logger,
	})
	if err != nil {
		return nil, fmt.Errorf("Error opening redis store: %w", err)
	}
	logger.Info("redis store enabled", "addr", rc.Addr, "tls", rc.TLS, "key_prefix", rc.KeyPrefix)
	return m, nil
}

// serve cancels the shared context when a listener fails.
func serve(ctx context.Context, cancel context.CancelFunc, logger *slog.Logger, name string, start func(context.Context) error) {
	if err := start(ctx); err != nil {
		logger.Error(name+" server error", "error", err)
		cancel()
	}
}

// tokenFingerprint names which credential is in use without disclosing it.
func tokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:4])
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

func nameservers(in []config.NameserverConfig) []dns.Nameserver {
	out := make([]dns.Nameserver, len(in))
	for i, ns := range in {
		out[i] = dns.Nameserver{Name: ns.Name, IP: ns.IP}
	}
	return out
}

// nameserverLabels lists the labels of the name servers below the domain.
func nameserverLabels(srv config.ServerConfig) map[string]bool {
	suffix := "." + strings.ToLower(strings.TrimSuffix(srv.Domain, "."))
	labels := make(map[string]bool)
	for _, ns := range srv.DNS.Nameservers {
		name := strings.ToLower(strings.TrimSuffix(ns.Name, "."))
		for _, label := range strings.Split(strings.TrimSuffix(name, suffix), ".") {
			labels[label] = true
		}
	}
	return labels
}

// avoidLabels keeps gen from issuing a reserved ID, so no collector can
// attribute a name server's traffic to a hook.
func avoidLabels(gen func() string, reserved map[string]bool) func() string {
	return func() string {
		for {
			if id := gen(); !reserved[id] {
				return id
			}
		}
	}
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
	fmt.Println("Hookd - High-performance DNS/HTTP/SMTP interaction server")
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
	fmt.Println()
	fmt.Println("  # Enable inbound mail capture (requires server.smtp.enabled)")
	fmt.Println("  hookd --config config.yaml --smtp-port 25")
}
