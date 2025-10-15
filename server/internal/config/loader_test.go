package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

func TestLoad_DefaultConfig(t *testing.T) {
	// Reset viper
	viper.Reset()

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("failed to load default config: %v", err)
	}

	if cfg.Server.Domain == "" {
		t.Error("expected domain to be set")
	}

	if cfg.Server.DNS.Port != 53 {
		t.Errorf("expected DNS port 53, got %d", cfg.Server.DNS.Port)
	}
}

func TestLoad_FromFile(t *testing.T) {
	// Create a temporary config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `
server:
  domain: "test.example.com"
  dns:
    port: 5353
  http:
    port: 8080
  api:
    auth_token: "test-token"
`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	// Reset viper
	viper.Reset()

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("failed to load config from file: %v", err)
	}

	if cfg.Server.Domain != "test.example.com" {
		t.Errorf("expected domain test.example.com, got %s", cfg.Server.Domain)
	}

	if cfg.Server.DNS.Port != 5353 {
		t.Errorf("expected DNS port 5353, got %d", cfg.Server.DNS.Port)
	}

	if cfg.Server.HTTP.Port != 8080 {
		t.Errorf("expected HTTP port 8080, got %d", cfg.Server.HTTP.Port)
	}

	if cfg.Server.API.AuthToken != "test-token" {
		t.Errorf("expected auth token test-token, got %s", cfg.Server.API.AuthToken)
	}
}

func TestLoad_InvalidFile(t *testing.T) {
	viper.Reset()

	_, err := Load("/nonexistent/config.yaml")
	if err == nil {
		t.Error("expected error for nonexistent config file")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "invalid.yaml")

	if err := os.WriteFile(configPath, []byte("invalid: yaml: content:"), 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	viper.Reset()

	_, err := Load(configPath)
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}

func TestLoad_ValidationError(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Config with empty domain (invalid)
	configContent := `
server:
  domain: ""
`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	viper.Reset()

	_, err := Load(configPath)
	if err == nil {
		t.Error("expected validation error for empty domain")
	}
}

func TestRegisterFlags(t *testing.T) {
	// Reset flags
	pflag.CommandLine = pflag.NewFlagSet(os.Args[0], pflag.ContinueOnError)

	RegisterFlags()

	if pflag.Lookup("config") == nil {
		t.Error("expected config flag to be registered")
	}

	if pflag.Lookup("token") == nil {
		t.Error("expected token flag to be registered")
	}

	if pflag.Lookup("domain") == nil {
		t.Error("expected domain flag to be registered")
	}

	if pflag.Lookup("dns-port") == nil {
		t.Error("expected dns-port flag to be registered")
	}

	if pflag.Lookup("http-port") == nil {
		t.Error("expected http-port flag to be registered")
	}

	if pflag.Lookup("https-port") == nil {
		t.Error("expected https-port flag to be registered")
	}

	if pflag.Lookup("version") == nil {
		t.Error("expected version flag to be registered")
	}

	if pflag.Lookup("help") == nil {
		t.Error("expected help flag to be registered")
	}
}

func TestApplyFlags(t *testing.T) {
	viper.Reset()
	cfg := DefaultConfig()

	// Set viper values (simulating CLI flags)
	viper.Set("token", "flag-token")
	viper.Set("domain", "flag.example.com")
	viper.Set("dns-port", 5353)
	viper.Set("http-port", 8080)
	viper.Set("https-port", 8443)

	applyFlags(cfg)

	if cfg.Server.API.AuthToken != "flag-token" {
		t.Errorf("expected auth token flag-token, got %s", cfg.Server.API.AuthToken)
	}

	if cfg.Server.Domain != "flag.example.com" {
		t.Errorf("expected domain flag.example.com, got %s", cfg.Server.Domain)
	}

	if cfg.Server.DNS.Port != 5353 {
		t.Errorf("expected DNS port 5353, got %d", cfg.Server.DNS.Port)
	}

	if cfg.Server.HTTP.Port != 8080 {
		t.Errorf("expected HTTP port 8080, got %d", cfg.Server.HTTP.Port)
	}

	if cfg.Server.HTTPS.Port != 8443 {
		t.Errorf("expected HTTPS port 8443, got %d", cfg.Server.HTTPS.Port)
	}
}

func TestApplyFlags_NoOverride(t *testing.T) {
	viper.Reset()
	cfg := DefaultConfig()
	originalToken := cfg.Server.API.AuthToken
	originalDomain := cfg.Server.Domain

	// Don't set any flags
	applyFlags(cfg)

	// Values should remain unchanged
	if cfg.Server.API.AuthToken != originalToken {
		t.Errorf("expected token to remain %s, got %s", originalToken, cfg.Server.API.AuthToken)
	}

	if cfg.Server.Domain != originalDomain {
		t.Errorf("expected domain to remain %s, got %s", originalDomain, cfg.Server.Domain)
	}
}
