package config

import (
	"fmt"
	"strings"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// configKeys lists every configuration key, so environment variables can be
// bound explicitly. AutomaticEnv alone does not make nested keys visible to
// Unmarshal, so each key must be bound for env overrides to take effect.
var configKeys = []string{
	"server.domain",
	"server.dns.enabled",
	"server.dns.port",
	"server.http.port",
	"server.https.enabled",
	"server.https.port",
	"server.https.autocert",
	"server.https.cache_dir",
	"server.api.auth_token",
	"eviction.interaction_ttl",
	"eviction.hook_ttl",
	"eviction.max_per_hook",
	"eviction.max_memory_mb",
	"eviction.cleanup_interval",
	"observability.metrics_enabled",
	"observability.log_level",
	"observability.log_format",
	"long_lived.enabled",
	"long_lived.max_ttl",
	"long_lived.max_hooks",
	"long_lived.max_interaction_body_bytes",
	"long_lived.max_metadata_bytes",
	"long_lived.db_path",
}

// Load loads configuration from file, environment, and CLI flags.
// Precedence, from lowest to highest: defaults < file < environment < flags.
func Load(configPath string) (*Config, error) {
	// Start with defaults
	cfg := DefaultConfig()

	// Set up environment variable overrides (HOOKD_SERVER_DOMAIN, etc.)
	// This must happen before Unmarshal so bound env values are applied.
	viper.SetEnvPrefix("HOOKD")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()
	for _, key := range configKeys {
		if err := viper.BindEnv(key); err != nil {
			return nil, fmt.Errorf("failed to bind env for %s: %w", key, err)
		}
	}

	// Load from YAML file if provided
	if configPath != "" {
		viper.SetConfigFile(configPath)
		if err := viper.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}
	}

	// Unmarshal file + environment values over the defaults. Keys absent from
	// both viper's config and the environment are left at their default value.
	if err := viper.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Apply CLI flags (highest priority)
	applyFlags(cfg)

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return cfg, nil
}

// RegisterFlags registers CLI flags
func RegisterFlags() {
	pflag.String("config", "", "Path to configuration file")
	pflag.String("token", "", "Override authentication token")
	pflag.String("domain", "", "Override server domain")
	pflag.Int("dns-port", 0, "Override DNS port")
	pflag.Int("http-port", 0, "Override HTTP port")
	pflag.Int("https-port", 0, "Override HTTPS port")
	pflag.Bool("version", false, "Show version information")
	pflag.BoolP("help", "h", false, "Show help message")
}

// applyFlags applies CLI flag overrides to the configuration
func applyFlags(cfg *Config) {
	if token := viper.GetString("token"); token != "" {
		cfg.Server.API.AuthToken = token
	}

	if domain := viper.GetString("domain"); domain != "" {
		cfg.Server.Domain = domain
	}

	if dnsPort := viper.GetInt("dns-port"); dnsPort > 0 {
		cfg.Server.DNS.Port = dnsPort
	}

	if httpPort := viper.GetInt("http-port"); httpPort > 0 {
		cfg.Server.HTTP.Port = httpPort
	}

	if httpsPort := viper.GetInt("https-port"); httpsPort > 0 {
		cfg.Server.HTTPS.Port = httpsPort
	}
}
