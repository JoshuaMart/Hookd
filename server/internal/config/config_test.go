package config

import (
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Server.Domain != "hookd.example.com" {
		t.Errorf("expected default domain hookd.example.com, got %s", cfg.Server.Domain)
	}

	if !cfg.Server.DNS.Enabled {
		t.Error("expected DNS to be enabled by default")
	}

	if cfg.Server.DNS.Port != 53 {
		t.Errorf("expected default DNS port 53, got %d", cfg.Server.DNS.Port)
	}

	if cfg.Eviction.InteractionTTL != 1*time.Hour {
		t.Errorf("expected interaction TTL 1h, got %v", cfg.Eviction.InteractionTTL)
	}

	if cfg.Eviction.MaxPerHook != 1000 {
		t.Errorf("expected default max_per_hook 1000, got %d", cfg.Eviction.MaxPerHook)
	}
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr bool
	}{
		{
			name:    "valid default config",
			modify:  func(c *Config) {},
			wantErr: false,
		},
		{
			name: "empty domain",
			modify: func(c *Config) {
				c.Server.Domain = ""
			},
			wantErr: true,
		},
		{
			name: "invalid DNS port",
			modify: func(c *Config) {
				c.Server.DNS.Port = 0
			},
			wantErr: true,
		},
		{
			name: "invalid HTTP port",
			modify: func(c *Config) {
				c.Server.HTTP.Port = 70000
			},
			wantErr: true,
		},
		{
			name: "negative TTL",
			modify: func(c *Config) {
				c.Eviction.InteractionTTL = -1 * time.Second
			},
			wantErr: true,
		},
		{
			name: "zero max per hook",
			modify: func(c *Config) {
				c.Eviction.MaxPerHook = 0
			},
			wantErr: true,
		},
		{
			name: "invalid log level",
			modify: func(c *Config) {
				c.Observability.LogLevel = "invalid"
			},
			wantErr: true,
		},
		{
			name: "invalid log format",
			modify: func(c *Config) {
				c.Observability.LogFormat = "xml"
			},
			wantErr: true,
		},
		{
			name: "autocert without cache dir",
			modify: func(c *Config) {
				c.Server.HTTPS.Enabled = true
				c.Server.HTTPS.AutoCert = true
				c.Server.HTTPS.CacheDir = ""
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.modify(cfg)

			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestConfig_EnsureAuthToken(t *testing.T) {
	t.Run("existing token", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Server.API.AuthToken = "existing-token"

		token, generated := cfg.EnsureAuthToken()

		if generated {
			t.Error("expected generated to be false")
		}

		if token != "existing-token" {
			t.Errorf("expected token existing-token, got %s", token)
		}
	})

	t.Run("generate token", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Server.API.AuthToken = ""

		token, generated := cfg.EnsureAuthToken()

		if !generated {
			t.Error("expected generated to be true")
		}

		if len(token) != 32 {
			t.Errorf("expected token length 32, got %d", len(token))
		}

		if cfg.Server.API.AuthToken != token {
			t.Error("expected token to be set in config")
		}
	})
}

func TestGenerateRandomToken(t *testing.T) {
	token1 := generateRandomToken()
	token2 := generateRandomToken()

	if len(token1) != 32 {
		t.Errorf("expected token length 32, got %d", len(token1))
	}

	if token1 == token2 {
		t.Error("expected tokens to be different")
	}
}
