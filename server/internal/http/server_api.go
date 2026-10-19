package http

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"

	"github.com/jomar/hookd/internal/config"
)

// startAPI starts the dedicated API listener once, when enabled. For acme it
// must run after startHTTPS, whose certificate it reuses. It binds before
// returning, so a taken port fails Start.
func (s *Server) startAPI(handler http.Handler, errChan chan<- error) error {
	listener := s.config.API.Listener
	if !listener.Enabled || s.apiServer != nil {
		return nil
	}
	tlsConfig, err := s.apiTLSConfig()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(listener.BindAddress, strconv.Itoa(listener.Port)))
	if err != nil {
		return fmt.Errorf("api server error: %w", err)
	}
	if tlsConfig == nil {
		s.logger.Warn("api listener serves plain HTTP: keep it behind a TLS-terminating proxy or mesh")
	}
	s.apiServer = newPublicServer(ln.Addr().String(), handler, s.logger)
	s.apiServer.TLSConfig = tlsConfig
	s.apiServer.ErrorLog = log.New(&apiErrorWriter{logger: s.logger}, "", 0)
	s.logger.Info("api listener starting",
		"port", listener.Port,
		"bind_address", listener.BindAddress,
		"tls", listener.TLS)
	go func() {
		var err error
		if tlsConfig != nil {
			err = s.apiServer.ServeTLS(ln, "", "")
		} else {
			err = s.apiServer.Serve(ln)
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
		reloader, err := newCertReloader(listener.CertFile, listener.KeyFile, s.logger)
		if err != nil {
			return nil, err
		}
		return &tls.Config{
			GetCertificate: reloader.GetCertificate,
			MinVersion:     tls.VersionTLS12,
			NextProtos:     []string{"h2", "http/1.1"},
		}, nil
	case config.APIListenerTLSNone:
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown api listener tls mode %q", listener.TLS)
	}
}
