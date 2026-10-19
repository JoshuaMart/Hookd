package main

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jomar/hookd/internal/config"
)

// loopbackPort reserves a port, released for the listener under test.
func loopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// selfSignedPair writes httptest's loopback certificate to disk and returns a
// client that trusts it.
func selfSignedPair(t *testing.T) (certFile, keyFile string, client *http.Client) {
	t.Helper()
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	defer ts.Close()
	cert := ts.TLS.Certificates[0]
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "api.crt"), filepath.Join(dir, "api.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, ts.Client()
}

// request sends one call and returns its status and body, retrying while the
// listener is still coming up.
func request(t *testing.T, client *http.Client, method, url, key string) (int, string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		req, _ := http.NewRequest(method, url, nil)
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		resp, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return resp.StatusCode, string(body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunWithAPIListener(t *testing.T) {
	for _, mode := range []string{config.APIListenerTLSNone, config.APIListenerTLSFile} {
		t.Run(mode, func(t *testing.T) {
			cfg := startupConfig(t)
			cfg.Server.HTTP.Port = loopbackPort(t)
			listener := &cfg.Server.API.Listener
			listener.Enabled, listener.BindAddress, listener.Port, listener.TLS = true, "127.0.0.1", loopbackPort(t), mode
			client, scheme := http.DefaultClient, "http"
			if mode == config.APIListenerTLSFile {
				listener.CertFile, listener.KeyFile, client = selfSignedPair(t)
				scheme = "https"
			}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}

			stop := make(chan os.Signal, 1)
			done := make(chan error, 1)
			go func() { done <- runUntil(cfg, stop) }()

			private := fmt.Sprintf("%s://127.0.0.1:%d", scheme, listener.Port)
			public := fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.HTTP.Port)
			if status, _ := request(t, client, http.MethodGet, private+"/healthz", ""); status != http.StatusOK {
				t.Fatalf("private /healthz: %d", status)
			}
			if status, _ := request(t, client, http.MethodPost, private+"/register", "test-startup-token"); status != http.StatusOK {
				t.Fatalf("private /register: %d", status)
			}
			if status, body := request(t, http.DefaultClient, http.MethodPost, public+"/register", "test-startup-token"); status != http.StatusOK || body != "" {
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
	cfg.Server.HTTP.Port = loopbackPort(t)
	cfg.Server.API.Listener = config.APIListenerConfig{Enabled: true, Port: loopbackPort(t), TLS: config.APIListenerTLSFile,
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
