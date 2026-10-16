package config

import (
	"testing"
	"time"

	"github.com/spf13/viper"
)

func TestDefaultConfig_Storage(t *testing.T) {
	s := DefaultConfig().Storage
	if s.Backend != StorageBackendLocal {
		t.Errorf("expected the local backend by default, got %q", s.Backend)
	}
	if s.Redis.KeyPrefix != "hookd:" || s.Redis.Addr == "" {
		t.Errorf("unexpected redis defaults: %+v", s.Redis)
	}
}

func TestConfig_ValidateStorage(t *testing.T) {
	redis := func(c *Config) { c.Storage.Backend = StorageBackendRedis }
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr bool
	}{
		{"local default", func(c *Config) {}, false},
		{"redis default", redis, false},
		{"unknown backend", func(c *Config) { c.Storage.Backend = "dynamodb" }, true},
		{"redis without addr", func(c *Config) { redis(c); c.Storage.Redis.Addr = "" }, true},
		{"redis without prefix", func(c *Config) { redis(c); c.Storage.Redis.KeyPrefix = "" }, true},
		{"redis zero read timeout", func(c *Config) { redis(c); c.Storage.Redis.ReadTimeout = 0 }, true},
		{"redis negative pool", func(c *Config) { redis(c); c.Storage.Redis.PoolSize = -1 }, true},
		{"redis cache disabled", func(c *Config) { redis(c); c.Storage.Redis.HasCacheSize = 0 }, false},
		{"redis negative cache ttl", func(c *Config) { redis(c); c.Storage.Redis.HasCacheTTL = -time.Second }, true},
		// The redis backend keeps long-lived hooks in Redis, not in SQLite.
		{"redis needs no db_path", func(c *Config) { redis(c); c.LongLived.DBPath = "" }, false},
		{"local needs db_path", func(c *Config) { c.LongLived.DBPath = "" }, true},
		// The redis backend keeps certificates in Redis, not in cache_dir.
		{"redis autocert needs no cache_dir", func(c *Config) {
			redis(c)
			c.Server.HTTPS.Enabled, c.Server.HTTPS.AutoCert, c.Server.HTTPS.CacheDir = true, true, ""
		}, false},
		{"local autocert needs cache_dir", func(c *Config) {
			c.Server.HTTPS.Enabled, c.Server.HTTPS.AutoCert, c.Server.HTTPS.CacheDir = true, true, ""
		}, true},
		// Ignored by the local backend, so not validated there.
		{"local ignores redis settings", func(c *Config) { c.Storage.Redis.Addr = "" }, false},
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

func TestLoad_StorageEnvOverride(t *testing.T) {
	viper.Reset()

	t.Setenv("HOOKD_STORAGE_BACKEND", "redis")
	t.Setenv("HOOKD_STORAGE_REDIS_ADDR", "cache.internal:6380")
	t.Setenv("HOOKD_STORAGE_REDIS_PASSWORD", "s3cret")
	t.Setenv("HOOKD_STORAGE_REDIS_TLS", "true")
	t.Setenv("HOOKD_STORAGE_REDIS_READ_TIMEOUT", "250ms")
	t.Setenv("HOOKD_STORAGE_REDIS_HAS_CACHE_TTL", "5s")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}
	r := cfg.Storage.Redis
	if cfg.Storage.Backend != StorageBackendRedis || r.Addr != "cache.internal:6380" || r.Password != "s3cret" ||
		!r.TLS || r.ReadTimeout != 250*time.Millisecond || r.HasCacheTTL != 5*time.Second {
		t.Errorf("env overrides not applied: backend=%q %+v", cfg.Storage.Backend, r)
	}
	if r.KeyPrefix != "hookd:" {
		t.Errorf("unset keys must keep their defaults, got prefix %q", r.KeyPrefix)
	}
}
