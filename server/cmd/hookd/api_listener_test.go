package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/testutil"
)

// call sends one request and returns its status and body.
func call(t *testing.T, client *http.Client, method, url, key string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// runOnFreePorts starts runUntil with fresh HTTP and API ports, retrying when
// another process takes one before the bind. It returns once both answer.
func runOnFreePorts(t *testing.T, cfg *config.Config, client *http.Client, scheme string) (stop chan os.Signal, done chan error) {
	t.Helper()
	for attempt := 0; attempt < 3; attempt++ {
		cfg.Server.HTTP.Port, cfg.Server.API.Listener.Port = testutil.FreePort(t), testutil.FreePort(t)
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		stop, done = make(chan os.Signal, 1), make(chan error, 1)
		go func() { done <- runUntil(cfg, stop) }()
		if answers(client, fmt.Sprintf("%s://127.0.0.1:%d/healthz", scheme, cfg.Server.API.Listener.Port), done) &&
			answers(http.DefaultClient, fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.Server.HTTP.Port), done) {
			return stop, done
		}
		stop <- os.Interrupt
		<-done
	}
	t.Fatal("hookd never came up on free ports")
	return nil, nil
}

// answers polls url until it returns 200, or until run exits.
func answers(client *http.Client, url string, done chan error) bool {
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		select {
		case err := <-done:
			done <- err
			return false
		default:
		}
		if resp, err := client.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
	}
	return false
}

func TestRunWithAPIListener(t *testing.T) {
	for _, mode := range []string{config.APIListenerTLSNone, config.APIListenerTLSFile} {
		t.Run(mode, func(t *testing.T) {
			cfg := startupConfig(t)
			listener := &cfg.Server.API.Listener
			listener.Enabled, listener.BindAddress, listener.Port, listener.TLS = true, "127.0.0.1", 8443, mode
			client, scheme := http.DefaultClient, "http"
			if mode == config.APIListenerTLSFile {
				var roots *x509.CertPool
				listener.CertFile, listener.KeyFile, roots = testutil.WriteKeyPair(t, "127.0.0.1")
				client = &http.Client{Timeout: time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
				scheme = "https"
			}
			stop, done := runOnFreePorts(t, cfg, client, scheme)
			private := fmt.Sprintf("%s://127.0.0.1:%d", scheme, listener.Port)
			public := fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.HTTP.Port)
			if status, _ := call(t, client, http.MethodPost, private+"/register", "test-startup-token"); status != http.StatusOK {
				t.Fatalf("private /register: %d", status)
			}
			if status, body := call(t, http.DefaultClient, http.MethodPost, public+"/register", "test-startup-token"); status != http.StatusOK || body != "" {
				t.Fatalf("public /register should look like capture, got %d: %q", status, body)
			}

			stop <- os.Interrupt
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("run did not stop")
			}
		})
	}
}

func TestRunStopsWhenAPIListenerFails(t *testing.T) {
	cfg := startupConfig(t)
	cfg.Server.HTTP.Port = testutil.FreePort(t)
	cfg.Server.API.Listener = config.APIListenerConfig{Enabled: true, Port: 8443, TLS: config.APIListenerTLSFile,
		CertFile: filepath.Join(t.TempDir(), "missing.crt"), KeyFile: filepath.Join(t.TempDir(), "missing.key")}
	done := make(chan error, 1)
	go func() { done <- runUntil(cfg, make(chan os.Signal)) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not stop after the API listener failed")
	}
}
