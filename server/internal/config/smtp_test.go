package config

import (
	"testing"
	"time"
)

func TestDefaultConfig_SMTP(t *testing.T) {
	smtp := DefaultConfig().Server.SMTP

	// Opt-in: it binds a privileged port and exposes a public receiver, so an
	// upgrade must not start one without consent.
	if smtp.Enabled {
		t.Error("expected SMTP to be disabled by default")
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"port", smtp.Port, 25},
		{"max_message_bytes", smtp.MaxMessageBytes, 262144},
		{"max_recipients", smtp.MaxRecipients, 10},
		{"max_concurrent", smtp.MaxConcurrent, 50},
		{"read_timeout", smtp.ReadTimeout, 60 * time.Second},
		{"session_timeout", smtp.SessionTimeout, 5 * time.Minute},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestConfig_ValidateSMTP(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr bool
	}{
		{
			name:    "disabled by default",
			modify:  func(c *Config) {},
			wantErr: false,
		},
		{
			name:    "enabled with defaults",
			modify:  func(c *Config) { c.Server.SMTP.Enabled = true },
			wantErr: false,
		},
		{
			// The limits are only meaningful when a listener runs, so a
			// disabled block is never validated.
			name: "nonsense limits are ignored while disabled",
			modify: func(c *Config) {
				c.Server.SMTP.Port = 0
				c.Server.SMTP.MaxRecipients = -1
			},
			wantErr: false,
		},
		{
			name: "port out of range",
			modify: func(c *Config) {
				c.Server.SMTP.Enabled = true
				c.Server.SMTP.Port = 70000
			},
			wantErr: true,
		},
		{
			name: "bind address is not an IP",
			modify: func(c *Config) {
				c.Server.SMTP.Enabled = true
				c.Server.SMTP.BindAddress = "not-an-ip"
			},
			wantErr: true,
		},
		{
			name: "empty bind address binds all interfaces",
			modify: func(c *Config) {
				c.Server.SMTP.Enabled = true
				c.Server.SMTP.BindAddress = ""
			},
			wantErr: false,
		},
		{
			name: "max_message_bytes not positive",
			modify: func(c *Config) {
				c.Server.SMTP.Enabled = true
				c.Server.SMTP.MaxMessageBytes = 0
			},
			wantErr: true,
		},
		{
			name: "max_recipients not positive",
			modify: func(c *Config) {
				c.Server.SMTP.Enabled = true
				c.Server.SMTP.MaxRecipients = 0
			},
			wantErr: true,
		},
		{
			name: "max_concurrent not positive",
			modify: func(c *Config) {
				c.Server.SMTP.Enabled = true
				c.Server.SMTP.MaxConcurrent = -1
			},
			wantErr: true,
		},
		{
			name: "read_timeout not positive",
			modify: func(c *Config) {
				c.Server.SMTP.Enabled = true
				c.Server.SMTP.ReadTimeout = 0
			},
			wantErr: true,
		},
		{
			name: "session_timeout not positive",
			modify: func(c *Config) {
				c.Server.SMTP.Enabled = true
				c.Server.SMTP.SessionTimeout = 0
			},
			wantErr: true,
		},
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
