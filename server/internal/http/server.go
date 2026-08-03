package http

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/jomar/hookd/internal/acme"
	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/eviction"
	"github.com/jomar/hookd/internal/storage"
)

// defaultACMEResolvers are the public recursive resolvers CertMagic uses to
// self-check DNS-01 challenge propagation when server.https.resolvers is unset
// (Cloudflare + Google, matching interactsh's defaults).
func defaultACMEResolvers() []string {
	return []string{
		"1.1.1.1:53",
		"1.0.0.1:53",
		"8.8.8.8:53",
		"8.8.4.4:53",
	}
}

// Deadlines for the public listeners: without them a slow request holds its
// socket and goroutine forever. readTimeout is the loosest since a capture body
// can reach several megabytes.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 64 * 1024
)

// shutdownTimeout bounds graceful shutdown.
const shutdownTimeout = 10 * time.Second

// Server represents an HTTP/HTTPS server
type Server struct {
	config        config.ServerConfig
	longLived     config.LongLivedConfig
	observability config.ObservabilityConfig

	storage      storage.Manager
	evictor      *eviction.Evictor
	acmeProvider *acme.Provider
	logger       *slog.Logger
	idGenerator  func() string
	httpServer   *http.Server
	httpsServer  *http.Server
}

// NewServer creates a new HTTP/HTTPS server
func NewServer(cfg config.ServerConfig, longLived config.LongLivedConfig, obs config.ObservabilityConfig, storage storage.Manager, evictor *eviction.Evictor, acmeProvider *acme.Provider, logger *slog.Logger, idGenerator func() string) *Server {
	return &Server{
		config:        cfg,
		longLived:     longLived,
		observability: obs,

		storage:      storage,
		evictor:      evictor,
		acmeProvider: acmeProvider,
		logger:       logger,
		idGenerator:  idGenerator,
	}
}

// routeByHost sends hook subdomains to the capture handler and everything else
// to the API mux. On paths alone, a callback to /register (or /poll, /activity,
// /metrics) would be answered by the API with a 401 and never recorded. The API
// is addressed on the apex domain, an IP, or a proxy hostname — none of which
// parse as a hook subdomain.
func routeByHost(apiMux http.Handler, capture *CaptureHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture.extractHookID(r.Host) != "" {
			capture.ServeHTTP(w, r)
			return
		}
		apiMux.ServeHTTP(w, r)
	})
}

// newPublicServer builds a listener with the shared deadline policy, so a new
// one cannot silently inherit net/http's zero values (meaning no deadline).
func newPublicServer(addr string, handler http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          newSuppressedTLSLogger(logger),
	}
}

// Start starts the HTTP/HTTPS servers
func (s *Server) Start(ctx context.Context) error {
	// Create handlers
	apiHandler := NewAPIHandler(s.storage, s.evictor, s.config.Domain, s.longLived, s.logger, s.idGenerator)
	captureHandler := NewCaptureHandler(s.storage, s.config.Domain, s.logger, s.idGenerator, s.evictor.MaxInteractionBodyBytes())

	// Create main mux
	mux := http.NewServeMux()

	// API endpoints. TLS is enforced ahead of auth so a plaintext request is
	// refused without its token ever being examined. The condition mirrors the
	// one guarding the HTTPS listener below: enforcing it while no HTTPS server
	// runs would leave the API unreachable.
	authMW := AuthMiddleware(s.config.API.AuthToken, s.logger)
	tlsMW := RequireTLSMiddleware(s.config.HTTPS.Enabled && s.config.HTTPS.AutoCert, s.logger)
	apiMW := func(h http.Handler) http.Handler { return tlsMW(authMW(h)) }

	mux.Handle("/register", apiMW(http.HandlerFunc(apiHandler.HandleRegister)))
	mux.Handle("/poll", apiMW(http.HandlerFunc(apiHandler.HandlePollBatch)))
	mux.Handle("/poll/", apiMW(http.HandlerFunc(apiHandler.HandlePoll)))
	mux.Handle("/activity", apiMW(http.HandlerFunc(apiHandler.HandleActivity)))

	// Metrics endpoint (no auth). Left unmounted when disabled, so the config
	// flag actually withholds the hook, interaction, eviction and memory counts.
	if s.observability.MetricsEnabled {
		mux.HandleFunc("/metrics", apiHandler.HandleMetrics)
	}

	// Wildcard capture (everything else)
	mux.Handle("/", captureHandler)

	// Apply global middleware
	handler := RecoveryMiddleware(s.logger)(LoggingMiddleware(s.logger)(routeByHost(mux, captureHandler)))

	errChan := make(chan error, 2)

	// Start HTTPS server if enabled
	if s.config.HTTPS.Enabled {
		if s.config.HTTPS.AutoCert {
			// Configure CertMagic with DNS-01 challenge using our custom provider
			s.logger.Info("configuring certmagic for wildcard certificate",
				"domain", s.config.Domain,
				"cache_dir", s.config.HTTPS.CacheDir)

			// Recursive resolvers CertMagic uses to self-check challenge
			// propagation. These are scoped to the ACME solver only — the
			// process's own name resolution is left on the system resolver, so a
			// correctly bound DNS server (see server.dns.bind_address) does not
			// require overriding net.DefaultResolver or stopping the host's stub
			// resolver.
			resolvers := s.config.HTTPS.Resolvers
			if len(resolvers) == 0 {
				resolvers = defaultACMEResolvers()
			}
			s.logger.Info("acme dns-01 self-check resolvers", "resolvers", resolvers)

			// Configure CertMagic defaults
			certmagic.DefaultACME.Agreed = true
			certmagic.DefaultACME.CA = certmagic.LetsEncryptProductionCA
			certmagic.DefaultACME.DisableHTTPChallenge = true
			certmagic.DefaultACME.DisableTLSALPNChallenge = true
			certmagic.DefaultACME.DNS01Solver = &certmagic.DNS01Solver{
				DNSManager: certmagic.DNSManager{
					DNSProvider: s.acmeProvider,
					Resolvers:   resolvers,
				},
			}

			// Create CertMagic config
			certmagicConfig := certmagic.NewDefault()
			certmagicConfig.Storage = &certmagic.FileStorage{Path: s.config.HTTPS.CacheDir}

			// Create ACME issuer with DNS-01 solver
			issuer := certmagic.NewACMEIssuer(certmagicConfig, certmagic.ACMEIssuer{
				CA:                      certmagic.LetsEncryptProductionCA,
				Agreed:                  true,
				DisableHTTPChallenge:    true,
				DisableTLSALPNChallenge: true,
				DNS01Solver: &certmagic.DNS01Solver{
					DNSManager: certmagic.DNSManager{
						DNSProvider: s.acmeProvider,
						Resolvers:   resolvers,
					},
				},
			})
			certmagicConfig.Issuers = []certmagic.Issuer{issuer}

			// Manage certificates for domain and wildcard
			domains := []string{s.config.Domain, "*." + s.config.Domain}

			s.logger.Info("obtaining wildcard certificate via DNS-01",
				"domains", domains,
				"cache_dir", s.config.HTTPS.CacheDir)

			// Obtain certificates synchronously
			err := certmagicConfig.ManageSync(context.Background(), domains)
			if err != nil {
				s.logger.Error("failed to obtain certificates", "error", err)
				return fmt.Errorf("failed to obtain certificates: %w", err)
			}

			s.logger.Info("wildcard certificate obtained successfully")

			// Get TLS config from CertMagic
			tlsConfig := certmagicConfig.TLSConfig()
			tlsConfig.NextProtos = append([]string{"h2", "http/1.1"}, tlsConfig.NextProtos...)

			s.httpsServer = newPublicServer(fmt.Sprintf(":%d", s.config.HTTPS.Port), handler, s.logger)
			s.httpsServer.TLSConfig = tlsConfig

			go func() {
				s.logger.Info("https server starting (certmagic wildcard)",
					"port", s.config.HTTPS.Port,
					"domains", domains)

				if err := s.httpsServer.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
					errChan <- fmt.Errorf("https server error: %w", err)
				}
			}()
		} else {
			s.logger.Warn("https enabled but autocert is false - manual TLS not yet implemented")
		}
	}

	// Always start HTTP server on configured port
	if s.httpServer == nil {
		s.httpServer = newPublicServer(fmt.Sprintf(":%d", s.config.HTTP.Port), handler, s.logger)

		go func() {
			s.logger.Info("http server starting", "port", s.config.HTTP.Port)
			if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errChan <- fmt.Errorf("http server error: %w", err)
			}
		}()
	}

	// Wait for context cancellation or error
	select {
	case <-ctx.Done():
		s.logger.Info("http server shutting down")

		// Draining is best-effort: a client that never finishes its request must
		// not keep the process alive.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if s.httpServer != nil {
			if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
				s.logger.Error("http server shutdown error", "error", err)
			}
		}
		if s.httpsServer != nil {
			if err := s.httpsServer.Shutdown(shutdownCtx); err != nil {
				s.logger.Error("https server shutdown error", "error", err)
			}
		}
		return nil
	case err := <-errChan:
		return err
	}
}

// suppressedTLSWriter wraps a logger to filter out TLS handshake errors
type suppressedTLSWriter struct {
	logger *slog.Logger
}

func (w *suppressedTLSWriter) Write(p []byte) (n int, err error) {
	msg := string(p)

	// Suppress TLS handshake errors (common from bots/scanners)
	if strings.Contains(msg, "TLS handshake error") ||
		strings.Contains(msg, "no certificate available") {
		return len(p), nil
	}

	// Log other errors through slog
	w.logger.Error("http server error", "message", strings.TrimSpace(msg))
	return len(p), nil
}

// newSuppressedTLSLogger creates a logger that suppresses TLS handshake errors
func newSuppressedTLSLogger(logger *slog.Logger) *log.Logger {
	return log.New(&suppressedTLSWriter{logger: logger}, "", 0)
}
