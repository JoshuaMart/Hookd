package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
)

func TestDefaultConfig_APIListener(t *testing.T) {
	l := DefaultConfig().Server.API.Listener
	if l.Enabled {
		t.Error("expected the API listener to be disabled by default")
	}
	if l.Port != 8443 || l.TLS != APIListenerTLSACME || l.BindAddress != "" {
		t.Errorf("unexpected API listener defaults: %+v", l)
	}
}

func TestConfig_ValidateAPIListener(t *testing.T) {
	enable := func(c *Config, mode string) {
		c.Server.API.Listener.Enabled = true
		c.Server.API.Listener.TLS = mode
	}
	autocert := func(c *Config) { c.Server.HTTPS.Enabled, c.Server.HTTPS.AutoCert = true, true }
	files := func(c *Config) { c.Server.API.Listener.CertFile, c.Server.API.Listener.KeyFile = "cert.pem", "key.pem" }
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr bool
	}{
		// Not validated while disabled, so v2.0.1 configs keep loading.
		{"disabled ignores settings", func(c *Config) { c.Server.API.Listener.Port = 0; c.Server.API.Listener.TLS = "bogus" }, false},
		{"none", func(c *Config) { enable(c, APIListenerTLSNone) }, false},
		{"acme", func(c *Config) { enable(c, APIListenerTLSACME); autocert(c) }, false},
		{"acme without https", func(c *Config) { enable(c, APIListenerTLSACME) }, true},
		{"acme without autocert", func(c *Config) { enable(c, APIListenerTLSACME); c.Server.HTTPS.Enabled = true }, true},
		{"file", func(c *Config) { enable(c, APIListenerTLSFile); files(c) }, false},
		{"file without cert", func(c *Config) { enable(c, APIListenerTLSFile); files(c); c.Server.API.Listener.CertFile = "" }, true},
		{"file without key", func(c *Config) { enable(c, APIListenerTLSFile); files(c); c.Server.API.Listener.KeyFile = "" }, true},
		{"unknown tls mode", func(c *Config) { enable(c, "mtls") }, true},
		{"empty tls mode", func(c *Config) { enable(c, "") }, true},
		{"port zero", func(c *Config) { enable(c, APIListenerTLSNone); c.Server.API.Listener.Port = 0 }, true},
		{"port too high", func(c *Config) { enable(c, APIListenerTLSNone); c.Server.API.Listener.Port = 65536 }, true},
		{"valid bind address", func(c *Config) { enable(c, APIListenerTLSNone); c.Server.API.Listener.BindAddress = "10.0.0.5" }, false},
		{"ipv6 bind address", func(c *Config) { enable(c, APIListenerTLSNone); c.Server.API.Listener.BindAddress = "fd00::5" }, false},
		{"hostname bind address", func(c *Config) { enable(c, APIListenerTLSNone); c.Server.API.Listener.BindAddress = "api.internal" }, true},
		{"same port as http", func(c *Config) { enable(c, APIListenerTLSNone); c.Server.API.Listener.Port = 80 }, true},
		{"same port as https", func(c *Config) { enable(c, APIListenerTLSACME); autocert(c); c.Server.API.Listener.Port = 443 }, true},
		{"https port free while disabled", func(c *Config) { enable(c, APIListenerTLSNone); c.Server.API.Listener.Port = 443 }, false},
		{"same port as smtp", func(c *Config) {
			enable(c, APIListenerTLSNone)
			c.Server.SMTP.Enabled = true
			c.Server.API.Listener.Port = 25
		}, true},
		{"smtp port free while disabled", func(c *Config) { enable(c, APIListenerTLSNone); c.Server.API.Listener.Port = 25 }, false},
		{"same port as dns", func(c *Config) { enable(c, APIListenerTLSNone); c.Server.API.Listener.Port = 53 }, true},
		{"dns port free while disabled", func(c *Config) {
			enable(c, APIListenerTLSNone)
			c.Server.DNS.Enabled = false
			c.Server.API.Listener.Port = 53
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.modify(cfg)
			if err := cfg.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoad_APIListenerFromFile(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `
server:
  api:
    auth_token: "file-token"
    listener:
      enabled: true
      bind_address: "10.0.0.5"
      port: 9443
      tls: "file"
      cert_file: "/etc/hookd/api.crt"
      key_file: "/etc/hookd/api.key"
`
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := APIListenerConfig{Enabled: true, BindAddress: "10.0.0.5", Port: 9443, TLS: APIListenerTLSFile,
		CertFile: "/etc/hookd/api.crt", KeyFile: "/etc/hookd/api.key"}
	if cfg.Server.API.Listener != want || cfg.Server.API.AuthToken != "file-token" {
		t.Errorf("unexpected API config: %+v", cfg.Server.API)
	}
}

func TestLoad_APIListenerEnvOverride(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	t.Setenv("HOOKD_SERVER_API_LISTENER_ENABLED", "true")
	t.Setenv("HOOKD_SERVER_API_LISTENER_BIND_ADDRESS", "10.0.0.7")
	t.Setenv("HOOKD_SERVER_API_LISTENER_PORT", "9000")
	t.Setenv("HOOKD_SERVER_API_LISTENER_TLS", "file")
	t.Setenv("HOOKD_SERVER_API_LISTENER_CERT_FILE", "/run/secrets/api.crt")
	t.Setenv("HOOKD_SERVER_API_LISTENER_KEY_FILE", "/run/secrets/api.key")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}
	want := APIListenerConfig{Enabled: true, BindAddress: "10.0.0.7", Port: 9000, TLS: APIListenerTLSFile,
		CertFile: "/run/secrets/api.crt", KeyFile: "/run/secrets/api.key"}
	if cfg.Server.API.Listener != want {
		t.Errorf("env overrides not applied: %+v", cfg.Server.API.Listener)
	}
}

func TestLoad_APIListenerInvalid(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	// The default acme mode needs autocert, which is off by default.
	t.Setenv("HOOKD_SERVER_API_LISTENER_ENABLED", "true")
	if _, err := Load(""); err == nil {
		t.Fatal("expected acme without autocert to be rejected")
	}
}
