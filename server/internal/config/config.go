package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Config represents the application configuration
type Config struct {
	Server        ServerConfig        `mapstructure:"server"`
	Eviction      EvictionConfig      `mapstructure:"eviction"`
	Observability ObservabilityConfig `mapstructure:"observability"`
	LongLived     LongLivedConfig     `mapstructure:"long_lived"`
}

// ServerConfig holds server-related configuration
type ServerConfig struct {
	Domain string      `mapstructure:"domain"`
	DNS    DNSConfig   `mapstructure:"dns"`
	HTTP   HTTPConfig  `mapstructure:"http"`
	HTTPS  HTTPSConfig `mapstructure:"https"`
	API    APIConfig   `mapstructure:"api"`
}

// DNSConfig holds DNS server configuration
type DNSConfig struct {
	Enabled bool `mapstructure:"enabled"`
	Port    int  `mapstructure:"port"`
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
}

// APIConfig holds API configuration
type APIConfig struct {
	AuthToken string `mapstructure:"auth_token"`
}

// EvictionConfig holds eviction-related configuration
type EvictionConfig struct {
	InteractionTTL  time.Duration `mapstructure:"interaction_ttl"`
	HookTTL         time.Duration `mapstructure:"hook_ttl"`
	MaxPerHook      int           `mapstructure:"max_per_hook"`
	MaxMemoryMB     int           `mapstructure:"max_memory_mb"`
	CleanupInterval time.Duration `mapstructure:"cleanup_interval"`
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

// ObservabilityConfig holds observability configuration
type ObservabilityConfig struct {
	MetricsEnabled bool   `mapstructure:"metrics_enabled"`
	LogLevel       string `mapstructure:"log_level"`
	LogFormat      string `mapstructure:"log_format"`
}

// DefaultConfig returns a configuration with sensible defaults
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Domain: "hookd.example.com",
			DNS: DNSConfig{
				Enabled: true,
				Port:    53,
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
			API: APIConfig{
				AuthToken: "",
			},
		},
		Eviction: EvictionConfig{
			InteractionTTL:  1 * time.Hour,
			HookTTL:         24 * time.Hour,
			MaxPerHook:      1000,
			MaxMemoryMB:     1800,
			CleanupInterval: 10 * time.Second,
		},
		Observability: ObservabilityConfig{
			MetricsEnabled: true,
			LogLevel:       "info",
			LogFormat:      "json",
		},
		LongLived: LongLivedConfig{
			Enabled:                 true,
			MaxTTL:                  720 * time.Hour, // 30 days
			MaxHooks:                500,
			MaxInteractionBodyBytes: 65536, // 64 KiB
			MaxMetadataBytes:        8192,  // 8 KiB
			DBPath:                  "/var/lib/hookd/longlived.db",
		},
	}
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	if c.Server.Domain == "" {
		return fmt.Errorf("server.domain is required")
	}

	if c.Server.DNS.Enabled && (c.Server.DNS.Port < 1 || c.Server.DNS.Port > 65535) {
		return fmt.Errorf("server.dns.port must be between 1 and 65535")
	}

	if c.Server.HTTP.Port < 1 || c.Server.HTTP.Port > 65535 {
		return fmt.Errorf("server.http.port must be between 1 and 65535")
	}

	if c.Server.HTTPS.Enabled && (c.Server.HTTPS.Port < 1 || c.Server.HTTPS.Port > 65535) {
		return fmt.Errorf("server.https.port must be between 1 and 65535")
	}

	if c.Server.HTTPS.Enabled && c.Server.HTTPS.AutoCert && c.Server.HTTPS.CacheDir == "" {
		return fmt.Errorf("server.https.cache_dir is required when autocert is enabled")
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
		if c.LongLived.DBPath == "" {
			return fmt.Errorf("long_lived.db_path is required when long_lived is enabled")
		}
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
