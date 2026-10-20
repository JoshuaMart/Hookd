package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

func TestValidate_DNS(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(c *Config)
		wantErr string
	}{
		{"several public_ips", func(c *Config) { c.Server.PublicIPs = []string{"203.0.113.7", "198.51.100.9"} }, ""},
		{"invalid public_ips entry", func(c *Config) { c.Server.PublicIPs = []string{"203.0.113.7", "not-an-ip"} }, "server.public_ips"},
		{"IPv6 public_ips entry", func(c *Config) { c.Server.PublicIPs = []string{"2001:db8::1"} }, "IPv4"},
		{"duplicate public_ips entry", func(c *Config) { c.Server.PublicIPs = []string{"203.0.113.7", "203.0.113.7"} }, "twice"},
		{"valid nameservers", func(c *Config) {
			c.Server.DNS.Nameservers = []NameserverConfig{
				{Name: "ns1.hookd.example.com", IP: "203.0.113.7"},
				{Name: "NS2.hookd.example.com.", IP: "198.51.100.9"},
			}
		}, ""},
		{"nameserver outside the domain", func(c *Config) {
			c.Server.DNS.Nameservers = []NameserverConfig{{Name: "ns1.other.com", IP: "203.0.113.7"}}
		}, "must be a name under"},
		{"nameserver at the apex", func(c *Config) {
			c.Server.DNS.Nameservers = []NameserverConfig{{Name: "hookd.example.com", IP: "203.0.113.7"}}
		}, "must be a name under"},
		{"nameserver suffix without a dot", func(c *Config) {
			c.Server.DNS.Nameservers = []NameserverConfig{{Name: "nshookd.example.com", IP: "203.0.113.7"}}
		}, "must be a name under"},
		{"duplicate nameserver", func(c *Config) {
			c.Server.DNS.Nameservers = []NameserverConfig{
				{Name: "ns1.hookd.example.com", IP: "203.0.113.7"},
				{Name: "NS1.hookd.example.com.", IP: "198.51.100.9"},
			}
		}, "twice"},
		{"nameserver without IP", func(c *Config) {
			c.Server.DNS.Nameservers = []NameserverConfig{{Name: "ns1.hookd.example.com"}}
		}, "IPv4"},
		{"nameserver with IPv6", func(c *Config) {
			c.Server.DNS.Nameservers = []NameserverConfig{{Name: "ns1.hookd.example.com", IP: "2001:db8::1"}}
		}, "IPv4"},
		{"zero soa_minimum_ttl", func(c *Config) { c.Server.DNS.SOAMinimumTTL = 0 }, "soa_minimum_ttl"},
		{"soa_minimum_ttl over 24h", func(c *Config) { c.Server.DNS.SOAMinimumTTL = 1200000 * time.Hour }, "soa_minimum_ttl"},
		{"zero max_tcp_connections", func(c *Config) { c.Server.DNS.MaxTCPConnections = 0 }, "max_tcp_connections"},
		{"zero rate_limit responses_per_second", func(c *Config) { c.Server.DNS.RateLimit.ResponsesPerSecond = 0 }, "rate_limit"},
		{"zero rate_limit burst", func(c *Config) { c.Server.DNS.RateLimit.Burst = 0 }, "rate_limit"},
		{"disabled rate_limit ignores its values", func(c *Config) {
			c.Server.DNS.RateLimit = DNSRateLimitConfig{Enabled: false}
		}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.modify(cfg)
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_DNSFromFile(t *testing.T) {
	viper.Reset()
	cfg, err := Load(writeConfig(t, `
server:
  domain: "oast.example.com"
  public_ips: ["203.0.113.7", "198.51.100.9"]
  dns:
    soa_minimum_ttl: 5m
    nameservers:
      - name: ns1.oast.example.com
        ip: 203.0.113.7
      - name: ns2.oast.example.com
        ip: 198.51.100.9
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Server.PublicIPs; len(got) != 2 || got[0] != "203.0.113.7" || got[1] != "198.51.100.9" {
		t.Errorf("public_ips = %v", got)
	}
	want := []NameserverConfig{{"ns1.oast.example.com", "203.0.113.7"}, {"ns2.oast.example.com", "198.51.100.9"}}
	if got := cfg.Server.DNS.Nameservers; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("nameservers = %+v", got)
	}
	if cfg.Server.DNS.SOAMinimumTTL != 5*time.Minute {
		t.Errorf("soa_minimum_ttl = %s", cfg.Server.DNS.SOAMinimumTTL)
	}
}

func TestLoad_DNSRateLimit(t *testing.T) {
	viper.Reset()
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if rl := cfg.Server.DNS.RateLimit; !rl.Enabled || rl.ResponsesPerSecond != DefaultRateLimitPerSecond || rl.Burst != DefaultRateLimitBurst {
		t.Errorf("default rate_limit = %+v", rl)
	}

	viper.Reset()
	t.Setenv("HOOKD_SERVER_DNS_RATE_LIMIT_ENABLED", "false")
	t.Setenv("HOOKD_SERVER_DNS_RATE_LIMIT_RESPONSES_PER_SECOND", "20")
	t.Setenv("HOOKD_SERVER_DNS_RATE_LIMIT_BURST", "40")
	if cfg, err = Load(""); err != nil {
		t.Fatal(err)
	}
	if rl := cfg.Server.DNS.RateLimit; rl != (DNSRateLimitConfig{Enabled: false, ResponsesPerSecond: 20, Burst: 40}) {
		t.Errorf("rate_limit from env = %+v", rl)
	}
}

func TestLoad_PublicIPsFromEnv(t *testing.T) {
	viper.Reset()
	t.Setenv("HOOKD_SERVER_PUBLIC_IPS", "203.0.113.7, 198.51.100.9")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Server.PublicIPs; len(got) != 2 || got[1] != "198.51.100.9" {
		t.Errorf("public_ips = %v", got)
	}
}

func TestLoad_RemovedPublicIP(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		viper.Reset()
		_, err := Load(writeConfig(t, "server:\n  public_ip: \"203.0.113.7\"\n"))
		if err == nil || !strings.Contains(err.Error(), "server.public_ips") {
			t.Fatalf("expected a pointer to server.public_ips, got %v", err)
		}
	})
	t.Run("env", func(t *testing.T) {
		viper.Reset()
		t.Setenv("HOOKD_SERVER_PUBLIC_IP", "203.0.113.7")
		if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "server.public_ips") {
			t.Fatalf("expected a pointer to server.public_ips, got %v", err)
		}
	})
}

func TestApplyFlags_PublicIPs(t *testing.T) {
	viper.Reset()
	cfg := DefaultConfig()
	viper.Set("public-ips", []string{"203.0.113.7", "198.51.100.9"})
	applyFlags(cfg)
	if got := cfg.Server.PublicIPs; len(got) != 2 || got[0] != "203.0.113.7" {
		t.Errorf("public_ips = %v", got)
	}
}
