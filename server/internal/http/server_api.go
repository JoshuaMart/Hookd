package http

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"github.com/jomar/hookd/internal/config"
)

// startAPI starts the dedicated API listener once, when enabled. For acme it
// must run after startHTTPS, whose certificate it reuses.
func (s *Server) startAPI(handler http.Handler, errChan chan<- error) error {
	listener := s.config.API.Listener
	if !listener.Enabled || s.apiServer != nil {
		return nil
	}
	tlsConfig, err := s.apiTLSConfig()
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(listener.BindAddress, strconv.Itoa(listener.Port))
	s.apiServer = newPublicServer(addr, handler, s.logger)
	s.apiServer.TLSConfig = tlsConfig
	if tlsConfig == nil {
		s.logger.Warn("api listener serves plain HTTP: keep it behind a TLS-terminating proxy or mesh")
	}
	go func() {
		s.logger.Info("api listener starting",
			"port", listener.Port,
			"bind_address", listener.BindAddress,
			"tls", listener.TLS)
		var err error
		if tlsConfig != nil {
			err = s.apiServer.ListenAndServeTLS("", "")
		} else {
			err = s.apiServer.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			errChan <- fmt.Errorf("api server error: %w", err)
		}
	}()
	return nil
}

// apiTLSConfig resolves the API listener's TLS mode; nil means plain HTTP.
func (s *Server) apiTLSConfig() (*tls.Config, error) {
	listener := s.config.API.Listener
	switch listener.TLS {
	case config.APIListenerTLSACME:
		if s.certTLS == nil {
			return nil, errors.New("api listener tls acme requires the certificate managed by the https listener")
		}
		return s.certTLS.Clone(), nil
	case config.APIListenerTLSFile:
		cert, err := tls.LoadX509KeyPair(listener.CertFile, listener.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load api listener certificate: %w", err)
		}
		return &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{"h2", "http/1.1"},
		}, nil
	case config.APIListenerTLSNone:
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown api listener tls mode %q", listener.TLS)
	}
}
