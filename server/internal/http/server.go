package http

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/caddyserver/certmagic"
	"github.com/jomar/hookd/internal/acme"
	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/eviction"
	"github.com/jomar/hookd/internal/storage"
)

// Server represents an HTTP/HTTPS server
type Server struct {
	config       config.ServerConfig
	storage      storage.Manager
	evictor      *eviction.Evictor
	acmeProvider *acme.Provider
	logger       *slog.Logger
	idGenerator  func() string
	httpServer   *http.Server
	httpsServer  *http.Server
}

// NewServer creates a new HTTP/HTTPS server
func NewServer(cfg config.ServerConfig, storage storage.Manager, evictor *eviction.Evictor, acmeProvider *acme.Provider, logger *slog.Logger, idGenerator func() string) *Server {
	return &Server{
		config:       cfg,
		storage:      storage,
		evictor:      evictor,
		acmeProvider: acmeProvider,
		logger:       logger,
		idGenerator:  idGenerator,
	}
}

// Start starts the HTTP/HTTPS servers
func (s *Server) Start(ctx context.Context) error {
	// Create handlers
	apiHandler := NewAPIHandler(s.storage, s.evictor, s.config.Domain, s.logger, s.idGenerator)
	captureHandler := NewCaptureHandler(s.storage, s.config.Domain, s.logger, s.idGenerator)

	// Create main mux
	mux := http.NewServeMux()

	// API endpoints (with auth)
	authMW := AuthMiddleware(s.config.API.AuthToken, s.logger)
	mux.Handle("/register", authMW(http.HandlerFunc(apiHandler.HandleRegister)))
	mux.Handle("/poll/", authMW(http.HandlerFunc(apiHandler.HandlePoll)))

	// Metrics endpoint (no auth)
	mux.HandleFunc("/metrics", apiHandler.HandleMetrics)

	// Wildcard capture (everything else)
	mux.Handle("/", captureHandler)

	// Apply global middleware
	handler := RecoveryMiddleware(s.logger)(LoggingMiddleware(s.logger)(mux))

	errChan := make(chan error, 2)

	// Start HTTPS server if enabled
	if s.config.HTTPS.Enabled {
		if s.config.HTTPS.AutoCert {
			// Override Go's default DNS resolver to use external nameservers
			// This prevents CertMagic from using system DNS (127.0.0.53:53)
			// which would query our own DNS server for Let's Encrypt domains
			net.DefaultResolver = &net.Resolver{
				PreferGo: true,
				Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
					d := net.Dialer{}
					// Use Google DNS for all resolution
					return d.DialContext(ctx, network, "8.8.8.8:53")
				},
			}

			s.logger.Info("configured global DNS resolver to use 8.8.8.8")

			// Configure CertMagic with DNS-01 challenge using our custom provider
			s.logger.Info("configuring certmagic for wildcard certificate",
				"domain", s.config.Domain,
				"cache_dir", s.config.HTTPS.CacheDir)

			// Default resolvers (like Interactsh)
			resolvers := []string{
				"1.1.1.1:53",
				"1.0.0.1:53",
				"8.8.8.8:53",
				"8.8.4.4:53",
			}

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

			s.httpsServer = &http.Server{
				Addr:      fmt.Sprintf(":%d", s.config.HTTPS.Port),
				Handler:   handler,
				TLSConfig: tlsConfig,
				ErrorLog:  newSuppressedTLSLogger(s.logger),
			}

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
		s.httpServer = &http.Server{
			Addr:     fmt.Sprintf(":%d", s.config.HTTP.Port),
			Handler:  handler,
			ErrorLog: newSuppressedTLSLogger(s.logger),
		}

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
		if s.httpServer != nil {
			if err := s.httpServer.Shutdown(context.Background()); err != nil {
				s.logger.Error("http server shutdown error", "error", err)
			}
		}
		if s.httpsServer != nil {
			if err := s.httpsServer.Shutdown(context.Background()); err != nil {
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
