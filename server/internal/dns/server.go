package dns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/libdns/libdns"
	"github.com/miekg/dns"

	"github.com/jomar/hookd/internal/acme"
	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/netutil"
	"github.com/jomar/hookd/internal/storage"
)

// Server represents a DNS server
type Server struct {
	domain       string
	publicIPs    []net.IP
	nameservers  map[string]net.IP // FQDN, lowercase, without the trailing dot
	nsNames      []string          // FQDNs in config order; the first is the SOA MNAME
	soaMinTTL    uint32
	maxTCPConns  int
	storage      storage.Manager
	acmeProvider *acme.Provider
	logger       *slog.Logger
	idGenerator  func() string
	addr         string
	udp          *dns.Server
	tcp          *dns.Server
}

// Nameserver is a delegated name server and the IPv4 of its glue record.
type Nameserver struct {
	Name string
	IP   string
}

// ServerOptions groups the DNS listener configuration and its dependencies.
type ServerOptions struct {
	Domain string
	Port   int
	// PublicIPs are returned together in A answers. When empty the outbound
	// interface IP is auto-detected, which is wrong behind NAT or on a
	// multi-homed host.
	PublicIPs []string
	// Nameservers answer NS at the apex; empty answers with the apex itself.
	Nameservers []Nameserver
	// SOAMinimumTTL bounds negative caching; zero means the config default.
	SOAMinimumTTL time.Duration
	// MaxTCPConnections caps open TCP connections; zero means the config default.
	MaxTCPConnections int
	// BindAddress is the local address the UDP and TCP listeners bind to; empty
	// binds all interfaces. Setting it to the host's public IP lets Hookd
	// coexist with a loopback stub resolver (e.g. systemd-resolved on
	// 127.0.0.53:53) instead of having to stop it.
	BindAddress  string
	Storage      storage.Manager
	ACMEProvider *acme.Provider
	Logger       *slog.Logger
	IDGenerator  func() string
}

const (
	answerTTL = 60
	// Plain UDP limit, and the EDNS0 size that avoids IP fragmentation.
	maxUDPSize = 1232
	// Fixed, so every instance reports the same serial.
	soaSerial = 1
)

// NewServer creates a new DNS server.
func NewServer(opts ServerOptions) (*Server, error) {
	domain := strings.ToLower(strings.TrimSuffix(opts.Domain, "."))

	ips := opts.PublicIPs
	if len(ips) == 0 {
		detected, err := getOutboundIP()
		if err != nil {
			return nil, fmt.Errorf("failed to detect server IP (set server.public_ips to override): %w", err)
		}
		if net.ParseIP(detected).To4() == nil {
			return nil, fmt.Errorf("auto-detected server IP %s is not IPv4: set server.public_ips", detected)
		}
		ips = []string{detected}
	}
	publicIPs := make([]net.IP, 0, len(ips))
	for _, raw := range ips {
		ip := net.ParseIP(raw).To4()
		if ip == nil {
			return nil, fmt.Errorf("public IP %q is not an IPv4 address", raw)
		}
		publicIPs = append(publicIPs, ip)
	}

	s := &Server{
		domain:       domain,
		publicIPs:    publicIPs,
		nameservers:  make(map[string]net.IP, len(opts.Nameservers)),
		soaMinTTL:    uint32(config.DefaultSOAMinimumTTL / time.Second),
		maxTCPConns:  config.DefaultMaxTCPConnections,
		storage:      opts.Storage,
		acmeProvider: opts.ACMEProvider,
		logger:       opts.Logger,
		idGenerator:  opts.IDGenerator,
		addr:         net.JoinHostPort(opts.BindAddress, strconv.Itoa(opts.Port)),
	}
	if opts.SOAMinimumTTL > 0 {
		s.soaMinTTL = uint32(opts.SOAMinimumTTL / time.Second)
	}
	if opts.MaxTCPConnections > 0 {
		s.maxTCPConns = opts.MaxTCPConnections
	}
	for _, ns := range opts.Nameservers {
		name := strings.ToLower(strings.TrimSuffix(ns.Name, "."))
		ip := net.ParseIP(ns.IP).To4()
		if ip == nil {
			return nil, fmt.Errorf("name server %s: %q is not an IPv4 address", ns.Name, ns.IP)
		}
		if netutil.HookIDFromHost(name, domain) == "" {
			return nil, fmt.Errorf("name server %s is not under %s", ns.Name, domain)
		}
		s.nameservers[name] = ip
		s.nsNames = append(s.nsNames, name)
	}

	mux := dns.NewServeMux()
	mux.HandleFunc(".", s.handleDNSRequest)
	s.udp = &dns.Server{Net: "udp", Handler: mux}
	s.tcp = &dns.Server{Net: "tcp", Handler: mux}

	return s, nil
}

// Start serves DNS over UDP and TCP on the same address until ctx is done.
func (s *Server) Start(ctx context.Context) error {
	pc, err := net.ListenPacket("udp", s.addr)
	if err != nil {
		return fmt.Errorf("dns udp listen: %w", err)
	}
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		_ = pc.Close()
		return fmt.Errorf("dns tcp listen: %w", err)
	}
	s.udp.PacketConn = pc
	s.tcp.Listener = newLimitListener(ln, s.maxTCPConns)

	s.logger.Info("dns server starting",
		"listen_addr", s.addr,
		"domain", s.domain,
		"public_ips", s.publicIPs,
		"nameservers", s.nsNames,
		"max_tcp_connections", s.maxTCPConns)

	// Shutdown refuses a server that has not started yet.
	started := make(chan struct{}, 2)
	errChan := make(chan error, 2)
	for _, srv := range []*dns.Server{s.udp, s.tcp} {
		srv.NotifyStartedFunc = func() { started <- struct{}{} }
		go func(srv *dns.Server) {
			if err := srv.ActivateAndServe(); err != nil {
				errChan <- fmt.Errorf("dns %s server error: %w", srv.Net, err)
			}
		}(srv)
	}
	for range 2 {
		select {
		case <-started:
		case err := <-errChan:
			_ = pc.Close()
			_ = ln.Close()
			return err
		}
	}

	select {
	case <-ctx.Done():
		s.logger.Info("dns server shutting down")
		return s.shutdown()
	case err := <-errChan:
		return errors.Join(err, s.shutdown())
	}
}

func (s *Server) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(s.udp.ShutdownContext(ctx), s.tcp.ShutdownContext(ctx))
}

// handleDNSRequest handles incoming DNS queries
func (s *Server) handleDNSRequest(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	inZone := false

	// Process each question
	for _, q := range r.Question {
		s.logger.Debug("dns query received",
			"qname", q.Name,
			"qtype", dns.TypeToString[q.Qtype],
			"client", w.RemoteAddr().String())

		// Check if this query is for our domain or a subdomain
		qnameLower := strings.ToLower(strings.TrimSuffix(q.Name, "."))
		isApex := qnameLower == s.domain
		isOurDomain := isApex || strings.HasSuffix(qnameLower, "."+s.domain)

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
		// After ACME: an SOA would let resolvers cache a challenge that is not yet published.
		inZone = inZone || isOurDomain

		// Extract hook ID from domain; name servers are never hooks.
		nsIP, isNameserver := s.nameservers[qnameLower]
		if hookID, found := s.resolveHookID(q.Name, isNameserver); found {
			// Log interaction
			sourceIP := netutil.ExtractIP(w.RemoteAddr().String())
			interaction := storage.DNSInteraction(
				s.idGenerator(),
				sourceIP,
				q.Name,
				dns.TypeToString[q.Qtype],
			)

			s.record(hookID, interaction)
		}

		// Respond based on query type
		switch q.Qtype {
		case dns.TypeA:
			ips := s.publicIPs
			// Same address as the parent's glue record.
			if isNameserver {
				ips = []net.IP{nsIP}
			}
			for _, ip := range ips {
				m.Answer = append(m.Answer, s.aRecord(q.Name, ip))
			}

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
					Ttl:    answerTTL,
				},
				Txt: []string{"hookd interaction server"},
			}
			m.Answer = append(m.Answer, rr)

		case dns.TypeNS:
			if isApex {
				s.answerNS(q.Name, m)
			}

		case dns.TypeSOA:
			if isApex {
				m.Answer = append(m.Answer, s.soa(q.Name))
			}

		case dns.TypeMX:
			rr := &dns.MX{
				Hdr: dns.RR_Header{
					Name:   q.Name,
					Rrtype: dns.TypeMX,
					Class:  dns.ClassINET,
					Ttl:    answerTTL,
				},
				Preference: 10,
				Mx:         s.domain + ".",
			}
			m.Answer = append(m.Answer, rr)

		default:
			// For other types, return empty answer
		}
	}

	// NODATA carries the SOA so resolvers can cache it (RFC 2308).
	if inZone && len(m.Answer) == 0 {
		m.Ns = append(m.Ns, s.soa(s.domain+"."))
	}
	fitReply(w, r, m)

	if err := w.WriteMsg(m); err != nil {
		s.logger.Error("failed to write dns response", "error", err)
	}
}

// answerNS lists the delegated name servers, with their glue as additional records.
func (s *Server) answerNS(qname string, m *dns.Msg) {
	names := s.nsNames
	if len(names) == 0 {
		names = []string{s.domain}
	}
	for _, name := range names {
		m.Answer = append(m.Answer, &dns.NS{
			Hdr: dns.RR_Header{Name: qname, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: answerTTL},
			Ns:  name + ".",
		})
		if ip, ok := s.nameservers[name]; ok {
			m.Extra = append(m.Extra, s.aRecord(name+".", ip))
		}
	}
}

func (s *Server) aRecord(name string, ip net.IP) *dns.A {
	return &dns.A{
		Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: answerTTL},
		A:   ip,
	}
}

func (s *Server) soa(name string) *dns.SOA {
	mname := s.domain + "."
	if len(s.nsNames) > 0 {
		mname = s.nsNames[0] + "."
	}
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: name, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: s.soaMinTTL},
		Ns:      mname,
		Mbox:    "hostmaster." + s.domain + ".",
		Serial:  soaSerial,
		Refresh: 3600,
		Retry:   600,
		Expire:  1209600,
		Minttl:  s.soaMinTTL,
	}
}

// fitReply echoes EDNS0 (RFC 6891 6.1.1) and truncates a UDP reply to the size
// the client accepts, setting TC so it retries over TCP.
func fitReply(w dns.ResponseWriter, r, m *dns.Msg) {
	size := dns.MinMsgSize
	if opt := r.IsEdns0(); opt != nil {
		size = min(max(int(opt.UDPSize()), dns.MinMsgSize), maxUDPSize)
		m.SetEdns0(maxUDPSize, false)
	}
	if w.LocalAddr().Network() == "udp" {
		m.Truncate(size)
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

// extractHookID extracts the hook ID from a domain name.
// Example: abc123.hookd.jomar.ovh -> abc123
func (s *Server) extractHookID(qname string) string {
	return netutil.HookIDFromHost(qname, s.domain)
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

// record stores a capture without waiting when the backend allows it: the
// answer does not depend on the write, and a shared store costs a round-trip.
func (s *Server) record(hookID string, interaction *storage.Interaction) {
	if async, ok := s.storage.(storage.AsyncRecorder); ok {
		async.RecordAsync(hookID, interaction)
		return
	}
	s.storage.AddInteraction(hookID, interaction)
}

func (s *Server) resolveHookID(qname string, isNameserver bool) (string, bool) {
	if isNameserver {
		return "", false
	}
	return netutil.ResolveHookID(qname, s.domain, s.firstExisting)
}

func (s *Server) firstExisting(ids ...string) int {
	return storage.FirstExisting(s.storage, ids...)
}
