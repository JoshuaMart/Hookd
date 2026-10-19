package http

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jomar/hookd/internal/testutil"
)

// leafName returns the first DNS name of the served certificate.
func leafName(t *testing.T, r *certReloader) string {
	t.Helper()
	cert, err := r.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.DNSNames[0]
}

// rotate overwrites the pair with one for name, dated in the future so the
// change is seen whatever the file system's timestamp resolution.
func rotate(t *testing.T, certFile, keyFile, name string, at time.Time) {
	t.Helper()
	certPEM, keyPEM := testutil.KeyPair(t, name)
	for path, data := range map[string][]byte{certFile: certPEM, keyFile: keyPEM} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCertReloader(t *testing.T) {
	certFile, keyFile, _ := testutil.WriteKeyPair(t, "first.test")
	var logs bytes.Buffer
	r, err := newCertReloader(certFile, keyFile, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if got := leafName(t, r); got != "first.test" {
		t.Fatalf("served %q", got)
	}

	// Within the interval, a rotation is not looked at yet.
	rotate(t, certFile, keyFile, "second.test", time.Now().Add(time.Hour))
	if got := leafName(t, r); got != "first.test" {
		t.Fatalf("reloaded before the interval: %q", got)
	}
	r.interval = 0
	if got := leafName(t, r); got != "second.test" {
		t.Fatalf("expected the rotated pair, got %q", got)
	}

	// A half-written pair keeps the current one and is retried.
	later := time.Now().Add(2 * time.Hour)
	if err := os.WriteFile(keyFile, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(keyFile, later, later); err != nil {
		t.Fatal(err)
	}
	if got := leafName(t, r); got != "second.test" || !strings.Contains(logs.String(), "reload failed") {
		t.Fatalf("expected the current pair kept with a warning, got %q: %s", got, logs.String())
	}
	rotate(t, certFile, keyFile, "third.test", later.Add(time.Hour))
	if got := leafName(t, r); got != "third.test" {
		t.Fatalf("expected the retry to load the fixed pair, got %q", got)
	}

	// A missing file keeps the current pair too.
	if err := os.Remove(certFile); err != nil {
		t.Fatal(err)
	}
	if got := leafName(t, r); got != "third.test" {
		t.Fatalf("expected the current pair kept, got %q", got)
	}
}

func TestNewCertReloaderErrors(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	certFile, keyFile, _ := testutil.WriteKeyPair(t, "valid.test")
	missing := filepath.Join(t.TempDir(), "missing.pem")
	for name, pair := range map[string][2]string{
		"missing cert":  {missing, keyFile},
		"missing key":   {certFile, missing},
		"mismatched":    {certFile, writeOtherKey(t)},
		"cert and cert": {certFile, certFile},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newCertReloader(pair[0], pair[1], logger); err == nil || !strings.Contains(err.Error(), "api listener certificate") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// writeOtherKey saves a key that does not match any other test certificate.
func writeOtherKey(t *testing.T) string {
	t.Helper()
	_, keyPEM := testutil.KeyPair(t, "other.test")
	path := filepath.Join(t.TempDir(), "other.key")
	if err := os.WriteFile(path, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
