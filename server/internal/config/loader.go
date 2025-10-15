package config

import (
	"fmt"
	"strings"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// Load loads configuration from file, environment, and CLI flags
func Load(configPath string) (*Config, error) {
	// Start with defaults
	cfg := DefaultConfig()

	// Load from YAML file if provided
	if configPath != "" {
		viper.SetConfigFile(configPath)
		if err := viper.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}

		if err := viper.Unmarshal(cfg); err != nil {
			return nil, fmt.Errorf("failed to unmarshal config: %w", err)
		}
	}

	// Environment variables override (HOOKD_SERVER_DOMAIN, etc.)
	viper.SetEnvPrefix("HOOKD")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

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
