package http

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/jomar/hookd/internal/acme"
	"github.com/jomar/hookd/internal/config"
)

// Serial tests: production configures shared defaults. Block real issuance.
func tlsTestServer(t *testing.T) *Server {
	t.Helper()
	previousACME, previousEvent := certmagic.DefaultACME, certmagic.Default.OnEvent
	certmagic.Default.OnEvent = func(_ context.Context, event string, _ map[string]any) error {
		if event == "cert_obtaining" {
			return errors.New("ACME issuance disabled in tests")
		}
		return nil
	}
	t.Cleanup(func() { certmagic.DefaultACME = previousACME; certmagic.Default.OnEvent = previousEvent })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Server{logger: logger, acmeProvider: acme.NewProvider(logger), config: config.ServerConfig{
		Domain: fmt.Sprintf("coverage-%d.test", time.Now().UnixNano()),
		HTTPS:  config.HTTPSConfig{Enabled: true, AutoCert: true, Port: -1, CacheDir: t.TempDir()},
	}}
}

func TestCertMagicDNSConfiguration(t *testing.T) {
	for _, resolvers := range [][]string{nil, {"127.0.0.1:5353"}} {
		t.Run(fmt.Sprint(resolvers), func(t *testing.T) {
			s := tlsTestServer(t)
			s.config.HTTPS.Resolvers = resolvers
			cfg := s.newCertMagicConfig()
			disk, ok := cfg.Storage.(*certmagic.FileStorage)
			if !ok || disk.Path != s.config.HTTPS.CacheDir {
				t.Fatalf("unexpected storage: %#v", cfg.Storage)
			}
			if len(cfg.Issuers) != 1 {
				t.Fatalf("issuer count=%d", len(cfg.Issuers))
			}
			issuer, ok := cfg.Issuers[0].(*certmagic.ACMEIssuer)
			if !ok {
				t.Fatalf("unexpected issuer %T", cfg.Issuers[0])
			}
			if issuer.CA != certmagic.LetsEncryptProductionCA || !issuer.Agreed || !issuer.DisableHTTPChallenge || !issuer.DisableTLSALPNChallenge {
				t.Fatal("DNS-only issuer policy changed")
			}
			solver, ok := issuer.DNS01Solver.(*certmagic.DNS01Solver)
			if !ok {
				t.Fatalf("unexpected solver %T", issuer.DNS01Solver)
			}
			want := resolvers
			if len(want) == 0 {
				want = defaultACMEResolvers()
			}
			if solver.DNSProvider != s.acmeProvider || !reflect.DeepEqual(solver.Resolvers, want) {
				t.Fatalf("unexpected DNS solver: %+v", solver)
			}
		})
	}
}

func TestHTTPSCertificateFailureIsReported(t *testing.T) {
	s := tlsTestServer(t)
	err := s.startHTTPS(http.NewServeMux(), make(chan error, 1))
	if err == nil || !strings.Contains(err.Error(), "failed to obtain certificates") || !strings.Contains(err.Error(), "ACME issuance disabled in tests") {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.httpsServer != nil {
		t.Fatal("listener created despite certificate failure")
	}
}

// Preload a local certificate as managed: ManageSync uses it without contacting
// an issuer. It has no OCSP URLs and remains valid well beyond the test run.
func cacheTestCertificate(t *testing.T, s *Server) *x509.CertPool {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{s.config.Domain, "*." + s.config.Domain},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cfg := s.newCertMagicConfig()
	issuer := cfg.Issuers[0].IssuerKey()
	ctx := context.Background()
	for path, data := range map[string][]byte{
		certmagic.StorageKeys.SiteCert(issuer, s.config.Domain):       certPEM,
		certmagic.StorageKeys.SitePrivateKey(issuer, s.config.Domain): keyPEM,
		certmagic.StorageKeys.SiteMeta(issuer, s.config.Domain):       []byte(`{}`),
	} {
		if err := cfg.Storage.Store(ctx, path, data); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cfg.CacheManagedCertificate(ctx, s.config.Domain); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("invalid test certificate")
	}
	return roots
}

func TestCachedCertificateSupportsVerifiedTLS(t *testing.T) {
	s := tlsTestServer(t)
	roots := cacheTestCertificate(t, s)
	cfg, err := s.obtainTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []string{"h2", "http/1.1"} {
		if !slices.Contains(cfg.NextProtos, protocol) {
			t.Fatalf("missing ALPN %s", protocol)
		}
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(handleHealth))
	server.TLS = cfg
	server.StartTLS()
	defer server.Close()
	for _, name := range []string{s.config.Domain, "hook." + s.config.Domain} {
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: name}}
		client := &http.Client{Transport: transport, Timeout: time.Second}
		resp, err := client.Get(server.URL + "/health")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		transport.CloseIdleConnections()
		if err != nil || resp.StatusCode != 200 || !strings.Contains(string(body), `"status":"ok"`) {
			t.Fatalf("health over TLS: status=%d, body=%s, err=%v", resp.StatusCode, body, err)
		}
	}
	errs := make(chan error, 1)
	if err := s.startHTTPS(http.NewServeMux(), errs); err != nil {
		t.Fatal(err)
	}
	defer s.httpsServer.Close()
	select {
	case err := <-errs:
		if !strings.Contains(err.Error(), "https server error") {
			t.Fatalf("unexpected listener error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTPS listener failure was not reported")
	}
}
