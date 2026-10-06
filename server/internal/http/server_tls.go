package http

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"

	"github.com/caddyserver/certmagic"
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

// startHTTPS prepares certificates before starting the HTTPS listener.
func (s *Server) startHTTPS(handler http.Handler, errChan chan<- error) error {
	if !s.config.HTTPS.Enabled {
		return nil
	}
	if !s.config.HTTPS.AutoCert {
		s.logger.Warn("https enabled but autocert is false - manual TLS not yet implemented")
		return nil
	}
	tlsConfig, err := s.obtainTLSConfig()
	if err != nil {
		return err
	}
	s.httpsServer = newPublicServer(fmt.Sprintf(":%d", s.config.HTTPS.Port), handler, s.logger)
	s.httpsServer.TLSConfig = tlsConfig
	go func() {
		s.logger.Info("https server starting (certmagic wildcard)",
			"port", s.config.HTTPS.Port,
			"domains", []string{s.config.Domain, "*." + s.config.Domain})
		if err := s.httpsServer.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			errChan <- fmt.Errorf("https server error: %w", err)
		}
	}()
	return nil
}

// obtainTLSConfig synchronously obtains the domain and wildcard certificates.
func (s *Server) obtainTLSConfig() (*tls.Config, error) {
	certmagicConfig := s.newCertMagicConfig()
	// Manage certificates for domain and wildcard
	domains := []string{s.config.Domain, "*." + s.config.Domain}

	s.logger.Info("obtaining wildcard certificate via DNS-01",
		"domains", domains,
		"cache_dir", s.config.HTTPS.CacheDir)

	// Obtain certificates synchronously
	err := certmagicConfig.ManageSync(context.Background(), domains)
	if err != nil {
		s.logger.Error("failed to obtain certificates", "error", err)
		return nil, fmt.Errorf("failed to obtain certificates: %w", err)
	}

	s.logger.Info("wildcard certificate obtained successfully")

	// Get TLS config from CertMagic
	tlsConfig := certmagicConfig.TLSConfig()
	tlsConfig.NextProtos = append([]string{"h2", "http/1.1"}, tlsConfig.NextProtos...)

	return tlsConfig, nil
}

// newCertMagicConfig configures DNS-01 issuance and its dedicated resolvers.
func (s *Server) newCertMagicConfig() *certmagic.Config {
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

	return certmagicConfig
}
