package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// Config represents the application configuration
type Config struct {
	Server        ServerConfig        `mapstructure:"server"`
	Eviction      EvictionConfig      `mapstructure:"eviction"`
	Observability ObservabilityConfig `mapstructure:"observability"`
	LongLived     LongLivedConfig     `mapstructure:"long_lived"`
	Storage       StorageConfig       `mapstructure:"storage"`
}

// ServerConfig holds server-related configuration
type ServerConfig struct {
	Domain string `mapstructure:"domain"`
	// PublicIPs are the IPv4 addresses returned together in DNS A answers, one
	// per load balancer address. Empty auto-detects the outbound interface IP.
	PublicIPs []string    `mapstructure:"public_ips"`
	DNS       DNSConfig   `mapstructure:"dns"`
	HTTP      HTTPConfig  `mapstructure:"http"`
	HTTPS     HTTPSConfig `mapstructure:"https"`
	SMTP      SMTPConfig  `mapstructure:"smtp"`
	API       APIConfig   `mapstructure:"api"`
}

// DNSConfig holds DNS server configuration
type DNSConfig struct {
	Enabled bool `mapstructure:"enabled"`
	Port    int  `mapstructure:"port"`
	// BindAddress is the local address the DNS listener binds to. Leave empty to
	// bind all interfaces. Set it to the server's public IP to coexist with a
	// local stub resolver (e.g. systemd-resolved on 127.0.0.53:53) without having
	// to stop it — which would otherwise break the host's own DNS resolution.
	BindAddress string `mapstructure:"bind_address"`
	// Nameservers are the delegated NS names, each with its glue IP. Empty
	// answers NS with the apex itself.
	Nameservers []NameserverConfig `mapstructure:"nameservers"`
	// SOAMinimumTTL is the SOA MINIMUM and TTL, which bounds negative caching.
	SOAMinimumTTL time.Duration `mapstructure:"soa_minimum_ttl"`
	// Connections beyond it are closed on accept.
	MaxTCPConnections int                `mapstructure:"max_tcp_connections"`
	RateLimit         DNSRateLimitConfig `mapstructure:"rate_limit"`
}

// DNSRateLimitConfig is response rate limiting (RRL) of UDP replies per source
// prefix (/24 IPv4, /56 IPv6). Queries are recorded either way; replies beyond
// the rate are truncated so real resolvers retry over TCP, which spoofed
// sources cannot.
type DNSRateLimitConfig struct {
	Enabled            bool `mapstructure:"enabled"`
	ResponsesPerSecond int  `mapstructure:"responses_per_second"`
	Burst              int  `mapstructure:"burst"`
}

// DNS defaults, shared with the DNS server for callers that leave them unset.
const (
	DefaultSOAMinimumTTL      = 60 * time.Second
	DefaultMaxTCPConnections  = 256
	DefaultRateLimitPerSecond = 100
	DefaultRateLimitBurst     = 500
)

// NameserverConfig is one delegated name server and the IPv4 of its glue record.
type NameserverConfig struct {
	Name string `mapstructure:"name"`
	IP   string `mapstructure:"ip"`
}

// HTTPConfig holds HTTP server configuration
type HTTPConfig struct {
	Port int `mapstructure:"port"`
}

// HTTPSConfig holds HTTPS server configuration
type HTTPSConfig struct {
	Enabled  bool   `mapstructure:"enabled"`
	Port     int    `mapstructure:"port"`
	AutoCert bool   `mapstructure:"autocert"`
	CacheDir string `mapstructure:"cache_dir"`
	// Resolvers are the recursive DNS servers CertMagic uses to self-check
	// DNS-01 challenge propagation before asking Let's Encrypt to validate. Leave
	// empty to use the public defaults (Cloudflare + Google). These are used only
	// by the ACME solver; the process's own name resolution is left untouched.
	Resolvers []string `mapstructure:"resolvers"`
}

// SMTPConfig configures inbound capture. Hookd receives only; it never relays.
type SMTPConfig struct {
	Enabled bool `mapstructure:"enabled"`
	Port    int  `mapstructure:"port"`
	// Empty binds all interfaces.
	BindAddress string `mapstructure:"bind_address"`
	// Advertised via the SIZE extension, so a sender can give up early.
	MaxMessageBytes int `mapstructure:"max_message_bytes"`
	MaxRecipients   int `mapstructure:"max_recipients"`
	MaxConcurrent   int `mapstructure:"max_concurrent"`
	// One command or data line, then the whole session. Both shorter than
	// RFC 5321 4.5.3.2 suggests, to bound what an idle attacker holds open.
	ReadTimeout    time.Duration `mapstructure:"read_timeout"`
	SessionTimeout time.Duration `mapstructure:"session_timeout"`
}

// APIConfig holds API configuration
type APIConfig struct {
	AuthToken string            `mapstructure:"auth_token"`
	Listener  APIListenerConfig `mapstructure:"listener"`
}

// TLS modes of the dedicated API listener.
const (
	APIListenerTLSACME = "acme"
	APIListenerTLSFile = "file"
	APIListenerTLSNone = "none"
)

// APIListenerConfig moves the API and /metrics off the public listeners onto a
// port of their own, meant to be reachable from a private network only.
type APIListenerConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// Empty binds all interfaces.
	BindAddress string `mapstructure:"bind_address"`
	Port        int    `mapstructure:"port"`
	// TLS is "acme" (the certificate managed for server.domain), "file"
	// (CertFile and KeyFile) or "none" (behind a TLS-terminating proxy).
	TLS      string `mapstructure:"tls"`
	CertFile string `mapstructure:"cert_file"`
	KeyFile  string `mapstructure:"key_file"`
}

// EvictionConfig holds eviction-related configuration
type EvictionConfig struct {
	InteractionTTL time.Duration `mapstructure:"interaction_ttl"`
	HookTTL        time.Duration `mapstructure:"hook_ttl"`
	MaxPerHook     int           `mapstructure:"max_per_hook"`
	// MaxInteractionBodyBytes caps a captured HTTP body. Ephemeral bodies are
	// held whole in memory, so with MaxPerHook this bounds a hook's footprint.
	MaxInteractionBodyBytes int           `mapstructure:"max_interaction_body_bytes"`
	MaxMemoryMB             int           `mapstructure:"max_memory_mb"`
	CleanupInterval         time.Duration `mapstructure:"cleanup_interval"`
}

// LongLivedConfig holds configuration for durable, long-lived hooks. These are
// persisted to a SQLite database so they survive restarts — the ephemeral,
// in-memory hooks are unaffected. A registration whose TTL exceeds the ephemeral
// hook TTL (eviction.hook_ttl) is treated as long-lived.
type LongLivedConfig struct {
	Enabled                 bool          `mapstructure:"enabled"`
	MaxTTL                  time.Duration `mapstructure:"max_ttl"`
	MaxHooks                int           `mapstructure:"max_hooks"`
	MaxInteractionBodyBytes int           `mapstructure:"max_interaction_body_bytes"`
	MaxMetadataBytes        int           `mapstructure:"max_metadata_bytes"`
	DBPath                  string        `mapstructure:"db_path"`
}

// Storage backends.
const (
	StorageBackendLocal = "local"
	StorageBackendRedis = "redis"
)

// StorageConfig selects where hooks and interactions live. The local backend
// keeps them in this process (memory, plus SQLite for long-lived hooks); the
// redis backend shares them between every instance behind a load balancer.
type StorageConfig struct {
	Backend string      `mapstructure:"backend"`
	Redis   RedisConfig `mapstructure:"redis"`
}

// RedisConfig configures the connection to a Redis or Valkey server. Cluster
// mode is not supported: the scripts touch per-hook and global keys together.
type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	TLS      bool   `mapstructure:"tls"`
	// KeyPrefix namespaces every key, so deployments can share a server.
	KeyPrefix    string        `mapstructure:"key_prefix"`
	DialTimeout  time.Duration `mapstructure:"dial_timeout"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
	// Zero keeps the client default (10 connections per CPU).
	PoolSize int `mapstructure:"pool_size"`
	// HasCacheSize hook IDs are remembered as existing for HasCacheTTL, which
	// spares most captures a round-trip. Zero disables the cache.
	HasCacheSize int           `mapstructure:"has_cache_size"`
	HasCacheTTL  time.Duration `mapstructure:"has_cache_ttl"`
}

// ObservabilityConfig holds observability configuration
type ObservabilityConfig struct {
	MetricsEnabled bool `mapstructure:"metrics_enabled"`
	// MetricsRequireAuth protects /metrics with the API token and TLS policy.
	MetricsRequireAuth bool   `mapstructure:"metrics_require_auth"`
	LogLevel           string `mapstructure:"log_level"`
	LogFormat          string `mapstructure:"log_format"`
}

// DefaultConfig returns a configuration with sensible defaults
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Domain: "hookd.example.com",
			DNS: DNSConfig{
				Enabled:           true,
				Port:              53,
				SOAMinimumTTL:     DefaultSOAMinimumTTL,
				MaxTCPConnections: DefaultMaxTCPConnections,
				RateLimit: DNSRateLimitConfig{
					Enabled:            true,
					ResponsesPerSecond: DefaultRateLimitPerSecond,
					Burst:              DefaultRateLimitBurst,
				},
			},
			HTTP: HTTPConfig{
				Port: 80,
			},
			HTTPS: HTTPSConfig{
				Enabled:  false,
				Port:     443,
				AutoCert: false,
				CacheDir: "/var/lib/hookd/certs",
			},
			SMTP: SMTPConfig{
				// Opt-in: it binds a privileged port and receives public mail.
				Enabled:         false,
				Port:            25,
				MaxMessageBytes: 262144, // 256 KiB
				MaxRecipients:   10,
				MaxConcurrent:   50,
				ReadTimeout:     60 * time.Second,
				SessionTimeout:  5 * time.Minute,
			},
			API: APIConfig{
				AuthToken: "",
				// Opt-in: off keeps the API on the public listeners.
				Listener: APIListenerConfig{
					Enabled: false,
					Port:    8443,
					TLS:     APIListenerTLSACME,
				},
			},
		},
		Eviction: EvictionConfig{
			InteractionTTL:          1 * time.Hour,
			HookTTL:                 24 * time.Hour,
			MaxPerHook:              1000,
			MaxInteractionBodyBytes: 1 << 20, // 1 MiB
			MaxMemoryMB:             1800,
			CleanupInterval:         10 * time.Second,
		},
		Observability: ObservabilityConfig{
			MetricsEnabled:     true,
			MetricsRequireAuth: false,
			LogLevel:           "info",
			LogFormat:          "json",
		},
		LongLived: LongLivedConfig{
			Enabled:                 true,
			MaxTTL:                  720 * time.Hour, // 30 days
			MaxHooks:                500,
			MaxInteractionBodyBytes: 65536, // 64 KiB
			MaxMetadataBytes:        8192,  // 8 KiB
			DBPath:                  "/var/lib/hookd/longlived.db",
		},
		Storage: StorageConfig{
			Backend: StorageBackendLocal,
			Redis: RedisConfig{
				Addr:         "localhost:6379",
				KeyPrefix:    "hookd:",
				DialTimeout:  5 * time.Second,
				ReadTimeout:  time.Second,
				WriteTimeout: time.Second,
				HasCacheSize: 50000,
				HasCacheTTL:  30 * time.Second,
			},
		},
	}
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	if c.Server.Domain == "" {
		return fmt.Errorf("server.domain is required")
	}

	if err := c.Server.validateDNS(); err != nil {
		return err
	}

	if c.Server.HTTP.Port < 1 || c.Server.HTTP.Port > 65535 {
		return fmt.Errorf("server.http.port must be between 1 and 65535")
	}

	if c.Server.HTTPS.Enabled && (c.Server.HTTPS.Port < 1 || c.Server.HTTPS.Port > 65535) {
		return fmt.Errorf("server.https.port must be between 1 and 65535")
	}

	// The redis backend keeps certificates in Redis instead.
	if c.Server.HTTPS.Enabled && c.Server.HTTPS.AutoCert && c.Server.HTTPS.CacheDir == "" &&
		c.Storage.Backend == StorageBackendLocal {
		return fmt.Errorf("server.https.cache_dir is required when autocert is enabled")
	}

	if c.Server.SMTP.Enabled {
		if c.Server.SMTP.Port < 1 || c.Server.SMTP.Port > 65535 {
			return fmt.Errorf("server.smtp.port must be between 1 and 65535")
		}
		if c.Server.SMTP.BindAddress != "" && net.ParseIP(c.Server.SMTP.BindAddress) == nil {
			return fmt.Errorf("server.smtp.bind_address must be a valid IP address")
		}
		if c.Server.SMTP.MaxMessageBytes <= 0 {
			return fmt.Errorf("server.smtp.max_message_bytes must be positive")
		}
		if c.Server.SMTP.MaxRecipients <= 0 {
			return fmt.Errorf("server.smtp.max_recipients must be positive")
		}
		if c.Server.SMTP.MaxConcurrent <= 0 {
			return fmt.Errorf("server.smtp.max_concurrent must be positive")
		}
		if c.Server.SMTP.ReadTimeout <= 0 {
			return fmt.Errorf("server.smtp.read_timeout must be positive")
		}
		if c.Server.SMTP.SessionTimeout <= 0 {
			return fmt.Errorf("server.smtp.session_timeout must be positive")
		}
	}

	if err := c.Server.validateAPIListener(); err != nil {
		return err
	}

	if c.Eviction.InteractionTTL <= 0 {
		return fmt.Errorf("eviction.interaction_ttl must be positive")
	}

	if c.Eviction.HookTTL <= 0 {
		return fmt.Errorf("eviction.hook_ttl must be positive")
	}

	if c.Eviction.MaxPerHook <= 0 {
		return fmt.Errorf("eviction.max_per_hook must be positive")
	}

	if c.Eviction.MaxInteractionBodyBytes <= 0 {
		return fmt.Errorf("eviction.max_interaction_body_bytes must be positive")
	}

	if c.Eviction.MaxMemoryMB <= 0 {
		return fmt.Errorf("eviction.max_memory_mb must be positive")
	}

	if c.Eviction.CleanupInterval <= 0 {
		return fmt.Errorf("eviction.cleanup_interval must be positive")
	}

	validLogLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLogLevels[c.Observability.LogLevel] {
		return fmt.Errorf("observability.log_level must be one of: debug, info, warn, error")
	}

	validLogFormats := map[string]bool{"json": true, "text": true}
	if !validLogFormats[c.Observability.LogFormat] {
		return fmt.Errorf("observability.log_format must be one of: json, text")
	}

	// Metadata can be attached to ephemeral hooks too, so its cap must always be
	// valid regardless of whether the long-lived store is enabled.
	if c.LongLived.MaxMetadataBytes <= 0 {
		return fmt.Errorf("long_lived.max_metadata_bytes must be positive")
	}

	if c.LongLived.Enabled {
		// A long-lived hook is one whose TTL exceeds the ephemeral hook TTL, so
		// the cap must leave room above it, otherwise nothing could ever qualify.
		if c.LongLived.MaxTTL <= c.Eviction.HookTTL {
			return fmt.Errorf("long_lived.max_ttl must be greater than eviction.hook_ttl")
		}
		if c.LongLived.MaxHooks <= 0 {
			return fmt.Errorf("long_lived.max_hooks must be positive")
		}
		if c.LongLived.MaxInteractionBodyBytes <= 0 {
			return fmt.Errorf("long_lived.max_interaction_body_bytes must be positive")
		}
		// The redis backend stores long-lived hooks alongside ephemeral ones.
		if c.LongLived.DBPath == "" && c.Storage.Backend == StorageBackendLocal {
			return fmt.Errorf("long_lived.db_path is required when long_lived is enabled")
		}
	}

	return c.Storage.validate()
}

// validateDNS checks the answer IPs and the DNS listener settings.
func (s ServerConfig) validateDNS() error {
	seen := make(map[string]bool, len(s.PublicIPs))
	for _, ip := range s.PublicIPs {
		if !isIPv4(ip) {
			return fmt.Errorf("server.public_ips: %q is not a valid IPv4 address", ip)
		}
		if seen[ip] {
			return fmt.Errorf("server.public_ips: %q is listed twice", ip)
		}
		seen[ip] = true
	}

	d := s.DNS
	if d.Enabled && (d.Port < 1 || d.Port > 65535) {
		return fmt.Errorf("server.dns.port must be between 1 and 65535")
	}
	if d.BindAddress != "" && net.ParseIP(d.BindAddress) == nil {
		return fmt.Errorf("server.dns.bind_address must be a valid IP address")
	}
	if d.SOAMinimumTTL < time.Second || d.SOAMinimumTTL > 24*time.Hour {
		return fmt.Errorf("server.dns.soa_minimum_ttl must be between 1s and 24h")
	}
	if d.MaxTCPConnections <= 0 {
		return fmt.Errorf("server.dns.max_tcp_connections must be positive")
	}
	if rl := d.RateLimit; rl.Enabled && (rl.ResponsesPerSecond <= 0 || rl.Burst <= 0) {
		return fmt.Errorf("server.dns.rate_limit.responses_per_second and burst must be positive")
	}

	zone := "." + strings.ToLower(strings.TrimSuffix(s.Domain, "."))
	names := make(map[string]bool, len(d.Nameservers))
	for _, ns := range d.Nameservers {
		name := strings.ToLower(strings.TrimSuffix(ns.Name, "."))
		// In-bailiwick only: the parent zone can hold glue for names below the delegation.
		if _, ok := dns.IsDomainName(name); !ok || !strings.HasSuffix(name, zone) {
			return fmt.Errorf("server.dns.nameservers: %q must be a name under %s", ns.Name, s.Domain)
		}
		if names[name] {
			return fmt.Errorf("server.dns.nameservers: %q is listed twice", ns.Name)
		}
		names[name] = true
		if !isIPv4(ns.IP) {
			return fmt.Errorf("server.dns.nameservers: %q needs a valid IPv4 address, got %q", ns.Name, ns.IP)
		}
	}
	return nil
}

func isIPv4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil
}

// validateAPIListener checks the dedicated API listener, when enabled.
func (s ServerConfig) validateAPIListener() error {
	l := s.API.Listener
	if !l.Enabled {
		return nil
	}
	if l.Port < 1 || l.Port > 65535 {
		return fmt.Errorf("server.api.listener.port must be between 1 and 65535")
	}
	if l.BindAddress != "" && net.ParseIP(l.BindAddress) == nil {
		return fmt.Errorf("server.api.listener.bind_address must be a valid IP address")
	}
	// DNS also listens on TCP, so its port collides as well.
	for _, other := range []struct {
		name    string
		enabled bool
		port    int
	}{
		{"server.http.port", true, s.HTTP.Port},
		{"server.https.port", s.HTTPS.Enabled, s.HTTPS.Port},
		{"server.smtp.port", s.SMTP.Enabled, s.SMTP.Port},
		{"server.dns.port", s.DNS.Enabled, s.DNS.Port},
	} {
		if other.enabled && other.port == l.Port {
			return fmt.Errorf("server.api.listener.port must differ from %s", other.name)
		}
	}
	switch l.TLS {
	case APIListenerTLSACME:
		if !s.HTTPS.Enabled || !s.HTTPS.AutoCert {
			return fmt.Errorf("server.api.listener.tls acme requires server.https.enabled and server.https.autocert")
		}
	case APIListenerTLSFile:
		if l.CertFile == "" || l.KeyFile == "" {
			return fmt.Errorf("server.api.listener.tls file requires cert_file and key_file")
		}
	case APIListenerTLSNone:
	default:
		return fmt.Errorf("server.api.listener.tls must be one of: acme, file, none")
	}
	return nil
}

func (s StorageConfig) validate() error {
	switch s.Backend {
	case StorageBackendLocal:
		return nil
	case StorageBackendRedis:
	default:
		return fmt.Errorf("storage.backend must be one of: local, redis")
	}

	r := s.Redis
	if r.Addr == "" {
		return fmt.Errorf("storage.redis.addr is required when storage.backend is redis")
	}
	if r.KeyPrefix == "" {
		return fmt.Errorf("storage.redis.key_prefix must not be empty")
	}
	if r.DialTimeout <= 0 || r.ReadTimeout <= 0 || r.WriteTimeout <= 0 {
		return fmt.Errorf("storage.redis dial_timeout, read_timeout and write_timeout must be positive")
	}
	if r.PoolSize < 0 {
		return fmt.Errorf("storage.redis.pool_size must not be negative")
	}
	if r.HasCacheSize < 0 || r.HasCacheTTL < 0 {
		return fmt.Errorf("storage.redis has_cache_size and has_cache_ttl must not be negative")
	}
	return nil
}

// EnsureAuthToken ensures an auth token exists, generating one if needed
func (c *Config) EnsureAuthToken() (string, bool) {
	if c.Server.API.AuthToken != "" {
		return c.Server.API.AuthToken, false
	}

	// Generate a random token
	token := generateRandomToken()
	c.Server.API.AuthToken = token
	return token, true
}

// generateRandomToken generates a random 32-character hex token
func generateRandomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("failed to generate random token: %v", err))
	}
	return hex.EncodeToString(b)
}
