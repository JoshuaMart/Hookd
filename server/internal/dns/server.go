package dns

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/libdns/libdns"
	"github.com/miekg/dns"

	"github.com/jomar/hookd/internal/acme"
	"github.com/jomar/hookd/internal/storage"
)

// Server represents a DNS server
type Server struct {
	domain       string
	port         int
	serverIP     string
	storage      storage.Manager
	acmeProvider *acme.Provider
	logger       *slog.Logger
	idGenerator  func() string
	server       *dns.Server
}

// NewServer creates a new DNS server
func NewServer(domain string, port int, storage storage.Manager, acmeProvider *acme.Provider, logger *slog.Logger, idGenerator func() string) (*Server, error) {
	// Auto-detect server IP
	serverIP, err := getOutboundIP()
	if err != nil {
		return nil, fmt.Errorf("failed to detect server IP: %w", err)
	}

	s := &Server{
		domain:       domain,
		port:         port,
		serverIP:     serverIP,
		storage:      storage,
		acmeProvider: acmeProvider,
		logger:       logger,
		idGenerator:  idGenerator,
	}

	// Create DNS server
	mux := dns.NewServeMux()
	mux.HandleFunc(".", s.handleDNSRequest)

	s.server = &dns.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Net:     "udp",
		Handler: mux,
	}

	return s, nil
}

// Start starts the DNS server
func (s *Server) Start(ctx context.Context) error {
	s.logger.Info("dns server starting",
		"port", s.port,
		"domain", s.domain,
		"server_ip", s.serverIP)

	errChan := make(chan error, 1)

	go func() {
		if err := s.server.ListenAndServe(); err != nil {
			errChan <- fmt.Errorf("dns server error: %w", err)
		}
	}()

	// Wait for context cancellation or error
	select {
	case <-ctx.Done():
		s.logger.Info("dns server shutting down")
		return s.server.Shutdown()
	case err := <-errChan:
		return err
	}
}

// handleDNSRequest handles incoming DNS queries
func (s *Server) handleDNSRequest(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true

	// Process each question
	for _, q := range r.Question {
		s.logger.Debug("dns query received",
			"qname", q.Name,
			"qtype", dns.TypeToString[q.Qtype],
			"client", w.RemoteAddr().String())

		// Check if this query is for our domain or a subdomain
		qnameLower := strings.ToLower(strings.TrimSuffix(q.Name, "."))
		domainLower := strings.ToLower(s.domain)
		isOurDomain := qnameLower == domainLower || strings.HasSuffix(qnameLower, "."+domainLower)

		// Normalize qname for case-insensitive comparison
		qnameLowerForCheck := strings.ToLower(q.Name)

		// Only respond to queries for our domain (except ACME challenges)
		if !isOurDomain && !(q.Qtype == dns.TypeTXT && strings.HasPrefix(qnameLowerForCheck, "_acme-challenge.")) {
			// Not our domain, skip this question
			s.logger.Debug("skipping query for external domain", "qname", q.Name)
			continue
		}

		// Check if this is an ACME TXT challenge (case-insensitive)
		if q.Qtype == dns.TypeTXT && strings.HasPrefix(qnameLowerForCheck, "_acme-challenge.") {
			s.logger.Info("acme challenge request detected", "qname", q.Name)
			if err := s.handleACMETXTChallenge(q.Name, m); err != nil {
				s.logger.Error("failed to handle acme challenge", "error", err, "qname", q.Name)
			}
			// Don't continue processing - just return the ACME response
			continue
		}

		// Extract hook ID from domain
		hookID := s.extractHookID(q.Name)
		if hookID != "" {
			// Log interaction
			sourceIP := extractIP(w.RemoteAddr().String())
			interaction := storage.DNSInteraction(
				s.idGenerator(),
				sourceIP,
				q.Name,
				dns.TypeToString[q.Qtype],
			)

			if err := s.storage.AddInteraction(hookID, interaction); err != nil {
				s.logger.Error("failed to store dns interaction", "error", err)
			}
		}

		// Respond based on query type
		switch q.Qtype {
		case dns.TypeA:
			rr := &dns.A{
				Hdr: dns.RR_Header{
					Name:   q.Name,
					Rrtype: dns.TypeA,
					Class:  dns.ClassINET,
					Ttl:    60,
				},
				A: net.ParseIP(s.serverIP),
			}
			m.Answer = append(m.Answer, rr)

		case dns.TypeAAAA:
			// Respond with empty answer for IPv6
			// Could add IPv6 support here if needed

		case dns.TypeTXT:
			s.logger.Info("responding with default TXT record",
				"qname", q.Name,
				"is_acme", strings.HasPrefix(q.Name, "_acme-challenge."),
				"response", "hookd interaction server")
			rr := &dns.TXT{
				Hdr: dns.RR_Header{
					Name:   q.Name,
					Rrtype: dns.TypeTXT,
					Class:  dns.ClassINET,
					Ttl:    60,
				},
				Txt: []string{"hookd interaction server"},
			}
			m.Answer = append(m.Answer, rr)

		case dns.TypeNS:
			rr := &dns.NS{
				Hdr: dns.RR_Header{
					Name:   q.Name,
					Rrtype: dns.TypeNS,
					Class:  dns.ClassINET,
					Ttl:    60,
				},
				Ns: s.domain + ".",
			}
			m.Answer = append(m.Answer, rr)

		case dns.TypeMX:
			rr := &dns.MX{
				Hdr: dns.RR_Header{
					Name:   q.Name,
					Rrtype: dns.TypeMX,
					Class:  dns.ClassINET,
					Ttl:    60,
				},
				Preference: 10,
				Mx:         s.domain + ".",
			}
			m.Answer = append(m.Answer, rr)

		default:
			// For other types, return empty answer
		}
	}

	if err := w.WriteMsg(m); err != nil {
		s.logger.Error("failed to write dns response", "error", err)
	}
}

// handleACMETXTChallenge handles ACME DNS-01 challenge TXT queries
func (s *Server) handleACMETXTChallenge(qname string, m *dns.Msg) error {
	// Normalize the query name to lowercase without trailing dot
	zoneName := strings.ToLower(strings.TrimSuffix(qname, "."))

	// Extract the zone from qname
	// For _acme-challenge.dns.bnty.ovh, the zone is bnty.ovh.
	// We need to figure out which zone this belongs to
	parts := strings.Split(zoneName, ".")
	if len(parts) < 2 {
		return fmt.Errorf("invalid qname: %s", qname)
	}

	// Try different zone combinations
	// For _acme-challenge.dns.bnty.ovh, try:
	// - bnty.ovh.
	// - dns.bnty.ovh.
	var records []libdns.Record
	var foundZone string

	for i := 1; i < len(parts); i++ {
		testZone := strings.Join(parts[i:], ".") + "."
		ctx := context.Background()
		recs, err := s.acmeProvider.GetRecords(ctx, testZone)
		if err == nil && len(recs) > 0 {
			// Found records for this zone
			for _, rec := range recs {
				rr := rec.RR()
				// Check if this record matches the query name
				recName := strings.ToLower(strings.TrimSuffix(rr.Name, "."))
				if recName == zoneName || rr.Name+"."+strings.TrimSuffix(testZone, ".") == zoneName {
					records = append(records, rec)
					foundZone = testZone
				}
			}
		}
	}

	if len(records) == 0 {
		s.logger.Warn("no acme records found", "qname", qname)
		return nil
	}

	s.logger.Info("acme challenge response",
		"qname", qname,
		"zone", foundZone,
		"record_count", len(records))

	// Add TXT records to response
	for _, record := range records {
		rr := record.RR()
		txtHdr := dns.RR_Header{
			Name:   qname,
			Rrtype: dns.TypeTXT,
			Class:  dns.ClassINET,
			Ttl:    uint32(rr.TTL.Seconds()), // Convert time.Duration to seconds
		}
		txtRecord := &dns.TXT{
			Hdr: txtHdr,
			Txt: []string{rr.Data},
		}
		m.Answer = append(m.Answer, txtRecord)

		s.logger.Info("acme challenge TXT record added",
			"qname", qname,
			"value", rr.Data,
			"total_answers", len(m.Answer))
	}

	return nil
}

// extractHookID extracts the hook ID from a domain name
// Example: abc123.hookd.jomar.ovh -> abc123
func (s *Server) extractHookID(qname string) string {
	// Remove trailing dot
	qname = strings.TrimSuffix(qname, ".")

	// Check if it's a subdomain of our domain
	suffix := "." + s.domain
	if !strings.HasSuffix(qname, suffix) {
		return ""
	}

	// Extract the subdomain part
	subdomain := strings.TrimSuffix(qname, suffix)

	// Handle multi-level subdomains (take the first part)
	parts := strings.Split(subdomain, ".")
	if len(parts) == 0 {
		return ""
	}

	return parts[0]
}

// getOutboundIP gets the preferred outbound IP of this machine
func getOutboundIP() (string, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "", err
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String(), nil
}

// extractIP extracts the IP address from a remote address string
func extractIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
