package http

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/storage"
	"github.com/jomar/hookd/internal/testutil"
)

// newListenerTestServer enables the dedicated API listener on loopback.
func newListenerTestServer(mode string, obs config.ObservabilityConfig) *Server {
	s := newObservabilityTestServer(obs, config.HTTPSConfig{})
	s.config.API.Listener = config.APIListenerConfig{Enabled: true, BindAddress: "127.0.0.1", TLS: mode}
	return s
}

// serveTest runs one request through handler.
func serveTest(handler http.Handler, method, host, path, key string, viaTLS bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Host = host
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	if viaTLS {
		req.TLS = &tls.ConnectionState{}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// waitForHealth polls url until it answers 200.
func waitForHealth(t *testing.T, client *http.Client, url string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never became healthy: %v", url, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// syncBuffer is a log sink safe to read while the listener writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// tlsClient trusts roots only.
func tlsClient(roots *x509.CertPool, serverName string) *http.Client {
	return &http.Client{Timeout: time.Second, Transport: &http.Transport{ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: serverName}}}
}

// apiURL is the address the started API listener actually bound.
func apiURL(s *Server, scheme string) string {
	return scheme + "://" + s.apiServer.Addr
}

func TestAPIListener_DisabledKeepsCombinedHandler(t *testing.T) {
	s := newObservabilityTestServer(config.ObservabilityConfig{MetricsEnabled: true}, config.HTTPSConfig{})
	public, private := s.newHandlers()
	if private != nil {
		t.Fatal("expected no private handler while the listener is disabled")
	}
	if w := serveTest(public, http.MethodPost, "example.com", "/register", "test-token", false); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id"`) {
		t.Fatalf("expected the API on the public listener, got %d: %s", w.Code, w.Body)
	}
}

func TestAPIListener_PublicHidesAPI(t *testing.T) {
	s := newListenerTestServer(config.APIListenerTLSNone, config.ObservabilityConfig{MetricsEnabled: true})
	public, _ := s.newHandlers()
	hook := s.storage.CreateHook("example.com", storage.CreateOptions{})
	s.storage.AddInteraction(hook.ID, storage.DNSInteraction("pending", "127.0.0.1", "q", "A"))
	before := s.storage.Stats()

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/register"},
		{http.MethodGet, "/poll/" + hook.ID},
		{http.MethodPost, "/poll"},
		{http.MethodGet, "/read?id=" + hook.ID},
		{http.MethodPost, "/ack"},
		{http.MethodGet, "/activity"},
		{http.MethodGet, "/hooks"},
		{http.MethodGet, "/metrics"},
	} {
		for _, host := range []string{"example.com", "203.0.113.10"} {
			for _, key := range []string{"", "test-token"} {
				t.Run(tc.method+tc.path+"/"+host+"/key="+key, func(t *testing.T) {
					w := serveTest(public, tc.method, host, tc.path, key, false)
					// Indistinguishable from any unknown path: no 401, no body.
					if w.Code != http.StatusOK || w.Body.Len() != 0 {
						t.Fatalf("expected the capture's empty 200, got %d: %q", w.Code, w.Body)
					}
				})
			}
		}
	}
	after := s.storage.Stats()
	if after.HooksActive != before.HooksActive || after.InteractionsTotal != before.InteractionsTotal {
		t.Fatalf("public requests reached storage: before=%+v after=%+v", before, after)
	}
}

func TestAPIListener_PublicStillCaptures(t *testing.T) {
	s := newListenerTestServer(config.APIListenerTLSNone, config.ObservabilityConfig{MetricsEnabled: true})
	public, _ := s.newHandlers()
	hook := s.storage.CreateHook("example.com", storage.CreateOptions{})
	for _, path := range []string{"/callback", "/register", "/metrics"} {
		if w := serveTest(public, http.MethodGet, hook.ID+".example.com", path, "", false); w.Code != http.StatusOK {
			t.Fatalf("%s: expected capture 200, got %d", path, w.Code)
		}
	}
	if got := len(s.storage.PollInteractions(hook.ID)); got != 3 {
		t.Fatalf("expected 3 captured interactions, got %d", got)
	}
}

func TestAPIListener_PrivateServesAPI(t *testing.T) {
	s := newListenerTestServer(config.APIListenerTLSNone, config.ObservabilityConfig{MetricsEnabled: true})
	_, private := s.newHandlers()

	if w := serveTest(private, http.MethodPost, "10.0.0.5:8443", "/register", "", false); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a key, got %d", w.Code)
	}
	w := serveTest(private, http.MethodPost, "10.0.0.5:8443", "/register", "test-token", false)
	if w.Code != http.StatusOK {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	var hook struct{ ID string }
	if err := json.Unmarshal(w.Body.Bytes(), &hook); err != nil || hook.ID == "" {
		t.Fatalf("unexpected register body %s: %v", w.Body, err)
	}
	s.storage.AddInteraction(hook.ID, storage.DNSInteraction("one", "127.0.0.1", "q", "A"))
	if w := serveTest(private, http.MethodGet, "example.com", "/poll/"+hook.ID, "", false); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on poll without a key, got %d", w.Code)
	}
	w = serveTest(private, http.MethodGet, "example.com", "/poll/"+hook.ID, "test-token", false)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"one"`) {
		t.Fatalf("poll: %d %s", w.Code, w.Body)
	}
	for _, path := range []string{"/activity", "/hooks"} {
		if w := serveTest(private, http.MethodGet, "example.com", path, "test-token", false); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
}

func TestAPIListener_Metrics(t *testing.T) {
	for _, tt := range []struct {
		name        string
		obs         config.ObservabilityConfig
		key         string
		wantPrivate int
	}{
		{"public metrics", config.ObservabilityConfig{MetricsEnabled: true}, "", http.StatusOK},
		{"protected without key", config.ObservabilityConfig{MetricsEnabled: true, MetricsRequireAuth: true}, "", http.StatusUnauthorized},
		{"protected with key", config.ObservabilityConfig{MetricsEnabled: true, MetricsRequireAuth: true}, "test-token", http.StatusOK},
		{"disabled", config.ObservabilityConfig{}, "test-token", http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newListenerTestServer(config.APIListenerTLSNone, tt.obs)
			public, private := s.newHandlers()
			if w := serveTest(public, http.MethodGet, "example.com", "/metrics", tt.key, false); w.Code != http.StatusOK || w.Body.Len() != 0 {
				t.Fatalf("expected no metrics on the public listener, got %d: %s", w.Code, w.Body)
			}
			w := serveTest(private, http.MethodGet, "example.com", "/metrics", tt.key, false)
			if w.Code != tt.wantPrivate {
				t.Fatalf("private /metrics = %d, want %d: %s", w.Code, tt.wantPrivate, w.Body)
			}
			if tt.wantPrivate == http.StatusOK && !strings.Contains(w.Body.String(), `"hooks"`) {
				t.Fatalf("expected metrics on the private listener: %s", w.Body)
			}
		})
	}
}

func TestAPIListener_PrivateNeverCaptures(t *testing.T) {
	s := newListenerTestServer(config.APIListenerTLSNone, config.ObservabilityConfig{MetricsEnabled: true})
	_, private := s.newHandlers()
	hook := s.storage.CreateHook("example.com", storage.CreateOptions{})
	for _, path := range []string{"/", "/callback", "/registerx"} {
		for _, host := range []string{hook.ID + ".example.com", "example.com"} {
			w := serveTest(private, http.MethodPost, host, path, "test-token", false)
			if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"error"`) {
				t.Fatalf("%s on %s: expected 404 JSON, got %d: %s", path, host, w.Code, w.Body)
			}
		}
	}
	if got := len(s.storage.PollInteractions(hook.ID)); got != 0 {
		t.Fatalf("private listener recorded %d interactions", got)
	}
}

func TestAPIListener_Health(t *testing.T) {
	s := newListenerTestServer(config.APIListenerTLSFile, config.ObservabilityConfig{})
	public, private := s.newHandlers()
	for name, handler := range map[string]http.Handler{"public": public, "private": private} {
		for _, path := range []string{"/health", "/healthz"} {
			// Liveness skips TLS enforcement; the listener itself may still be TLS-only.
			if w := serveTest(handler, http.MethodGet, "example.com", path, "", false); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok"`) {
				t.Fatalf("%s %s: %d %s", name, path, w.Code, w.Body)
			}
		}
	}
}

func TestAPIListener_TLSEnforcement(t *testing.T) {
	for _, tt := range []struct {
		mode   string
		viaTLS bool
		want   int
	}{
		{config.APIListenerTLSACME, false, http.StatusUpgradeRequired},
		{config.APIListenerTLSACME, true, http.StatusOK},
		{config.APIListenerTLSFile, false, http.StatusUpgradeRequired},
		{config.APIListenerTLSFile, true, http.StatusOK},
		{config.APIListenerTLSNone, false, http.StatusOK},
	} {
		t.Run(fmt.Sprintf("%s/tls=%t", tt.mode, tt.viaTLS), func(t *testing.T) {
			s := newListenerTestServer(tt.mode, config.ObservabilityConfig{})
			_, private := s.newHandlers()
			if w := serveTest(private, http.MethodPost, "example.com", "/register", "test-token", tt.viaTLS); w.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.want, w.Body)
			}
		})
	}
}

func TestStartAPI_PlainHTTP(t *testing.T) {
	s := newListenerTestServer(config.APIListenerTLSNone, config.ObservabilityConfig{})
	_, private := s.newHandlers()
	if err := s.startAPI(private, make(chan error, 1)); err != nil {
		t.Fatal(err)
	}
	defer s.apiServer.Close()
	waitForHealth(t, http.DefaultClient, apiURL(s, "http")+"/healthz")
	// Started once: a second call keeps the running listener.
	running := s.apiServer
	if err := s.startAPI(private, make(chan error, 1)); err != nil || s.apiServer != running {
		t.Fatalf("expected the running listener to be kept, err=%v", err)
	}
}

func TestStartAPI_File(t *testing.T) {
	certFile, keyFile, roots := testutil.WriteKeyPair(t, "127.0.0.1")
	s := newListenerTestServer(config.APIListenerTLSFile, config.ObservabilityConfig{})
	s.config.API.Listener.CertFile, s.config.API.Listener.KeyFile = certFile, keyFile
	_, private := s.newHandlers()
	if err := s.startAPI(private, make(chan error, 1)); err != nil {
		t.Fatal(err)
	}
	defer s.apiServer.Close()
	client := tlsClient(roots, "")
	base := apiURL(s, "https")
	waitForHealth(t, client, base+"/healthz")
	req, _ := http.NewRequest(http.MethodPost, base+"/register", nil)
	req.Header.Set("X-API-Key", "test-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ProtoMajor != 2 {
		t.Fatalf("register over TLS: %d %s", resp.StatusCode, resp.Proto)
	}
}

func TestStartAPI_ACMEReusesCertificate(t *testing.T) {
	s := tlsTestServer(t)
	logs := &syncBuffer{}
	s.logger = slog.New(slog.NewTextHandler(logs, nil))
	roots := cacheTestCertificate(t, s)
	errs := make(chan error, 2)
	// The HTTPS bind fails on port -1, but the certificate is obtained first.
	if err := s.startHTTPS(http.NewServeMux(), errs); err != nil {
		t.Fatal(err)
	}
	defer s.httpsServer.Close()
	if s.certTLS == nil || s.httpsServer.TLSConfig == s.certTLS {
		t.Fatal("expected the HTTPS listener to get its own clone of the certificate config")
	}
	s.config.API.Listener = config.APIListenerConfig{Enabled: true, BindAddress: "127.0.0.1", TLS: config.APIListenerTLSACME}
	if err := s.startAPI(http.HandlerFunc(handleHealth), errs); err != nil {
		t.Fatal(err)
	}
	defer s.apiServer.Close()
	if s.apiServer.TLSConfig == s.certTLS || s.apiServer.TLSConfig == s.httpsServer.TLSConfig {
		t.Fatal("expected the API listener to get its own clone of the certificate config")
	}
	waitForHealth(t, tlsClient(roots, s.config.Domain), apiURL(s, "https")+"/healthz")

	// A client with another SNI fails, and the listener says why.
	if _, err := tlsClient(roots, "internal-nlb.local").Get(apiURL(s, "https") + "/healthz"); err == nil {
		t.Fatal("expected the handshake to fail for an unknown SNI")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "clients must use server.domain") {
		if time.Now().After(deadline) {
			t.Fatalf("expected a warning for the unknown SNI, got: %s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartAPI_Errors(t *testing.T) {
	t.Run("acme without https", func(t *testing.T) {
		s := newListenerTestServer(config.APIListenerTLSACME, config.ObservabilityConfig{})
		if err := s.startAPI(http.NewServeMux(), make(chan error, 1)); err == nil || s.apiServer != nil {
			t.Fatalf("expected an error and no listener, got %v", err)
		}
	})
	t.Run("missing certificate", func(t *testing.T) {
		s := newListenerTestServer(config.APIListenerTLSFile, config.ObservabilityConfig{})
		s.config.API.Listener.CertFile = filepath.Join(t.TempDir(), "missing.crt")
		s.config.API.Listener.KeyFile = filepath.Join(t.TempDir(), "missing.key")
		if err := s.startAPI(http.NewServeMux(), make(chan error, 1)); err == nil || !strings.Contains(err.Error(), "api listener certificate") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("unknown mode", func(t *testing.T) {
		s := newListenerTestServer("mtls", config.ObservabilityConfig{})
		if err := s.startAPI(http.NewServeMux(), make(chan error, 1)); err == nil {
			t.Fatal("expected an unknown mode to be rejected")
		}
	})
	t.Run("disabled", func(t *testing.T) {
		s := newObservabilityTestServer(config.ObservabilityConfig{}, config.HTTPSConfig{})
		if err := s.startAPI(http.NewServeMux(), make(chan error, 1)); err != nil || s.apiServer != nil {
			t.Fatalf("expected nothing started, got %v", err)
		}
	})
	t.Run("bind failure", func(t *testing.T) {
		occupied, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer occupied.Close()
		s := newListenerTestServer(config.APIListenerTLSNone, config.ObservabilityConfig{})
		s.config.API.Listener.Port = occupied.Addr().(*net.TCPAddr).Port
		err = s.startAPI(http.NewServeMux(), make(chan error, 1))
		var opErr *net.OpError
		if !errors.As(err, &opErr) || !strings.Contains(err.Error(), "api server error") || s.apiServer != nil {
			t.Fatalf("expected a wrapped bind error and no listener, got %v", err)
		}
	})
}

// startWithFreePorts starts s on fresh ports, retrying when another process
// takes one between FreePort and the bind.
func startWithFreePorts(t *testing.T, s *Server) (stop func()) {
	t.Helper()
	for attempt := 0; attempt < 3; attempt++ {
		s.httpServer, s.apiServer = nil, nil
		s.config.HTTP.Port, s.config.API.Listener.Port = testutil.FreePort(t), testutil.FreePort(t)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Start(ctx) }()
		if healthy(fmt.Sprintf("http://127.0.0.1:%d/healthz", s.config.HTTP.Port), done) &&
			healthy(fmt.Sprintf("http://127.0.0.1:%d/healthz", s.config.API.Listener.Port), done) {
			return func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("server did not stop")
				}
			}
		}
		cancel()
		<-done
	}
	t.Fatal("server never came up on free ports")
	return nil
}

// healthy polls url until it answers 200, or until done reports Start exited.
func healthy(url string, done chan error) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			done <- err
			return false
		default:
		}
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestServer_StartWithAPIListener(t *testing.T) {
	s := newListenerTestServer(config.APIListenerTLSNone, config.ObservabilityConfig{MetricsEnabled: true})
	stop := startWithFreePorts(t, s)
	public := fmt.Sprintf("http://127.0.0.1:%d", s.config.HTTP.Port)
	private := fmt.Sprintf("http://127.0.0.1:%d", s.config.API.Listener.Port)

	resp, err := http.Get(public + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.ContentLength != 0 {
		t.Fatalf("expected no metrics on the public listener, got %d bytes", resp.ContentLength)
	}
	resp, err = http.Get(private + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("expected metrics on the private listener, got %d", resp.StatusCode)
	}

	stop()
	if _, err := http.Get(private + "/healthz"); err == nil {
		t.Fatal("expected the API listener to be closed")
	}
}

func TestServer_StartFailsOnAPIListenerTLS(t *testing.T) {
	s := newListenerTestServer(config.APIListenerTLSFile, config.ObservabilityConfig{})
	s.config.API.Listener.CertFile = filepath.Join(t.TempDir(), "missing.crt")
	s.config.API.Listener.KeyFile = s.config.API.Listener.CertFile
	if err := s.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "api listener certificate") {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.httpServer != nil {
		t.Fatal("expected the public listener not to start")
	}
}

func TestAPIErrorWriter(t *testing.T) {
	for _, tt := range []struct {
		msg   string
		level slog.Level
	}{
		{`http: TLS handshake error from 10.0.0.9:4242: no certificate available for 'internal-nlb.local'`, slog.LevelWarn},
		{"http: TLS handshake error from 10.0.0.9:4242: EOF", slog.LevelDebug},
		{"http: some other server error", slog.LevelError},
	} {
		var logs bytes.Buffer
		w := &apiErrorWriter{logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
		if n, err := w.Write([]byte(tt.msg + "\n")); err != nil || n != len(tt.msg)+1 {
			t.Fatalf("expected full write, got n=%d err=%v", n, err)
		}
		if !strings.Contains(logs.String(), "level="+tt.level.String()) {
			t.Errorf("%q: expected level %s, got %s", tt.msg, tt.level, logs.String())
		}
	}
}
