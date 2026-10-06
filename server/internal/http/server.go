package http

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jomar/hookd/internal/acme"
	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/eviction"
	"github.com/jomar/hookd/internal/storage"
)

// Deadlines for the public listeners: without them a slow request holds its
// socket and goroutine forever.
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

// ServerOptions groups listener configuration and runtime dependencies.
type ServerOptions struct {
	Server        config.ServerConfig
	LongLived     config.LongLivedConfig
	Observability config.ObservabilityConfig
	Storage       storage.Manager
	Evictor       *eviction.Evictor
	ACMEProvider  *acme.Provider
	Logger        *slog.Logger
	IDGenerator   func() string
}

// NewServer creates a new HTTP/HTTPS server.
func NewServer(opts ServerOptions) *Server {
	return &Server{
		config:        opts.Server,
		longLived:     opts.LongLived,
		observability: opts.Observability,
		storage:       opts.Storage,
		evictor:       opts.Evictor,
		acmeProvider:  opts.ACMEProvider,
		logger:        opts.Logger,
		idGenerator:   opts.IDGenerator,
	}
}

// newPublicServer applies the shared deadline policy, so a new listener cannot
// inherit net/http's zero values (meaning no deadline).
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

// Start starts the HTTP/HTTPS servers.
func (s *Server) Start(ctx context.Context) error {
	handler := s.newHandler()
	errChan := make(chan error, 2)
	if err := s.startHTTPS(handler, errChan); err != nil {
		return err
	}
	s.startHTTP(handler, errChan)
	select {
	case <-ctx.Done():
		s.shutdown()
		return nil
	case err := <-errChan:
		return err
	}
}

// startHTTP starts the plain HTTP listener once.
func (s *Server) startHTTP(handler http.Handler, errChan chan<- error) {
	if s.httpServer != nil {
		return
	}
	s.httpServer = newPublicServer(fmt.Sprintf(":%d", s.config.HTTP.Port), handler, s.logger)
	go func() {
		s.logger.Info("http server starting", "port", s.config.HTTP.Port)
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errChan <- fmt.Errorf("http server error: %w", err)
		}
	}()
}

// shutdown shares one bounded draining budget between the two listeners.
func (s *Server) shutdown() {
	s.logger.Info("http server shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, listener := range []struct {
		name   string
		server *http.Server
	}{{"http", s.httpServer}, {"https", s.httpsServer}} {
		if listener.server != nil {
			if err := listener.server.Shutdown(ctx); err != nil {
				s.logger.Error(listener.name+" server shutdown error", "error", err)
			}
		}
	}
}
