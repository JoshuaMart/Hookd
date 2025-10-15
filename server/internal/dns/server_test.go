package dns

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/libdns/libdns"
	"github.com/miekg/dns"

	"github.com/jomar/hookd/internal/acme"
	"github.com/jomar/hookd/internal/storage"
)

func TestNewServer(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	acmeProvider := acme.NewProvider(slog.Default())
	logger := slog.Default()

	server, err := NewServer("example.com", 5353, manager, acmeProvider, logger, idGen)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	if server.domain != "example.com" {
		t.Errorf("expected domain example.com, got %s", server.domain)
	}

	if server.port != 5353 {
		t.Errorf("expected port 5353, got %d", server.port)
	}

	if server.serverIP == "" {
		t.Error("expected server IP to be set")
	}
}

func TestServer_ExtractHookID(t *testing.T) {
	server := &Server{domain: "example.com"}

	tests := []struct {
		name     string
		qname    string
		expected string
	}{
		{"valid subdomain", "abc123.example.com.", "abc123"},
		{"no trailing dot", "abc123.example.com", "abc123"},
		{"exact domain", "example.com.", ""},
		{"multi-level subdomain", "sub.abc123.example.com.", "sub"},
		{"external domain", "other.com.", ""},
		{"no subdomain", "example.com", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := server.extractHookID(tt.qname)
			if result != tt.expected {
				t.Errorf("extractHookID(%q) = %q, want %q", tt.qname, result, tt.expected)
			}
		})
	}
}

func TestExtractIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		expected   string
	}{
		{"ipv4 with port", "192.168.1.1:12345", "192.168.1.1"},
		{"ipv6 with port", "[::1]:8080", "::1"},
		{"no port", "192.168.1.1", "192.168.1.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractIP(tt.remoteAddr)
			if result != tt.expected {
				t.Errorf("extractIP(%q) = %q, want %q", tt.remoteAddr, result, tt.expected)
			}
		})
	}
}

func TestGetOutboundIP(t *testing.T) {
	ip, err := getOutboundIP()
	if err != nil {
		t.Fatalf("failed to get outbound IP: %v", err)
	}

	if ip == "" {
		t.Error("expected non-empty IP")
	}

	// Verify it's a valid IP
	if net.ParseIP(ip) == nil {
		t.Errorf("invalid IP address: %s", ip)
	}
}

func TestServer_HandleDNSRequest_TypeA(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	acmeProvider := acme.NewProvider(slog.Default())
	logger := slog.Default()

	server, _ := NewServer("example.com", 5353, manager, acmeProvider, logger, idGen)

	// Create a hook
	hook := manager.CreateHook("example.com")

	// Create DNS query for A record
	m := new(dns.Msg)
	m.SetQuestion(hook.ID+".example.com.", dns.TypeA)

	// Create mock response writer
	w := &mockResponseWriter{remoteAddr: &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 12345}}

	server.handleDNSRequest(w, m)

	if w.msg == nil {
		t.Fatal("expected response message")
	}

	if len(w.msg.Answer) != 1 {
		t.Fatalf("expected 1 answer, got %d", len(w.msg.Answer))
	}

	aRecord, ok := w.msg.Answer[0].(*dns.A)
	if !ok {
		t.Fatal("expected A record")
	}

	if aRecord.A.String() != server.serverIP {
		t.Errorf("expected IP %s, got %s", server.serverIP, aRecord.A.String())
	}

	// Verify interaction was stored
	interactions, _ := manager.PollInteractions(hook.ID)
	if len(interactions) != 1 {
		t.Fatalf("expected 1 interaction, got %d", len(interactions))
	}

	if interactions[0].Type != storage.InteractionTypeDNS {
		t.Errorf("expected type dns, got %s", interactions[0].Type)
	}
}

func TestServer_HandleDNSRequest_TypeTXT(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	acmeProvider := acme.NewProvider(slog.Default())
	logger := slog.Default()

	server, _ := NewServer("example.com", 5353, manager, acmeProvider, logger, idGen)

	// Create DNS query for TXT record
	m := new(dns.Msg)
	m.SetQuestion("test.example.com.", dns.TypeTXT)

	w := &mockResponseWriter{remoteAddr: &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 12345}}

	server.handleDNSRequest(w, m)

	if w.msg == nil {
		t.Fatal("expected response message")
	}

	if len(w.msg.Answer) != 1 {
		t.Fatalf("expected 1 answer, got %d", len(w.msg.Answer))
	}

	txtRecord, ok := w.msg.Answer[0].(*dns.TXT)
	if !ok {
		t.Fatal("expected TXT record")
	}

	if len(txtRecord.Txt) != 1 || txtRecord.Txt[0] != "hookd interaction server" {
		t.Errorf("expected TXT 'hookd interaction server', got %v", txtRecord.Txt)
	}
}

func TestServer_HandleDNSRequest_ExternalDomain(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	acmeProvider := acme.NewProvider(slog.Default())
	logger := slog.Default()

	server, _ := NewServer("example.com", 5353, manager, acmeProvider, logger, idGen)

	// Create DNS query for external domain
	m := new(dns.Msg)
	m.SetQuestion("other.com.", dns.TypeA)

	w := &mockResponseWriter{remoteAddr: &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 12345}}

	server.handleDNSRequest(w, m)

	if w.msg == nil {
		t.Fatal("expected response message")
	}

	// Should have no answers for external domain
	if len(w.msg.Answer) != 0 {
		t.Fatalf("expected 0 answers for external domain, got %d", len(w.msg.Answer))
	}
}

func TestServer_HandleDNSRequest_TypeNS(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	acmeProvider := acme.NewProvider(slog.Default())
	logger := slog.Default()

	server, _ := NewServer("example.com", 5353, manager, acmeProvider, logger, idGen)

	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeNS)

	w := &mockResponseWriter{remoteAddr: &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 12345}}

	server.handleDNSRequest(w, m)

	if len(w.msg.Answer) != 1 {
		t.Fatalf("expected 1 answer, got %d", len(w.msg.Answer))
	}

	nsRecord, ok := w.msg.Answer[0].(*dns.NS)
	if !ok {
		t.Fatal("expected NS record")
	}

	if !strings.HasPrefix(nsRecord.Ns, server.domain) {
		t.Errorf("expected NS to contain domain %s, got %s", server.domain, nsRecord.Ns)
	}
}

func TestServer_HandleDNSRequest_TypeMX(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	acmeProvider := acme.NewProvider(slog.Default())
	logger := slog.Default()

	server, _ := NewServer("example.com", 5353, manager, acmeProvider, logger, idGen)

	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeMX)

	w := &mockResponseWriter{remoteAddr: &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 12345}}

	server.handleDNSRequest(w, m)

	if len(w.msg.Answer) != 1 {
		t.Fatalf("expected 1 answer, got %d", len(w.msg.Answer))
	}

	mxRecord, ok := w.msg.Answer[0].(*dns.MX)
	if !ok {
		t.Fatal("expected MX record")
	}

	if mxRecord.Preference != 10 {
		t.Errorf("expected preference 10, got %d", mxRecord.Preference)
	}
}

func TestServer_HandleACMETXTChallenge(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	acmeProvider := acme.NewProvider(slog.Default())
	logger := slog.Default()

	server, _ := NewServer("example.com", 5353, manager, acmeProvider, logger, idGen)

	t.Run("valid ACME challenge", func(t *testing.T) {
		// Add ACME record to provider
		ctx := context.Background()
		records := []libdns.Record{
			libdns.RR{
				Type: "TXT",
				Name: "_acme-challenge",
				Data: "test-acme-value",
				TTL:  time.Duration(300) * time.Second,
			},
		}
		acmeProvider.AppendRecords(ctx, "example.com.", records)

		// Create DNS message
		m := new(dns.Msg)

		// Call handleACMETXTChallenge
		err := server.handleACMETXTChallenge("_acme-challenge.example.com.", m)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(m.Answer) != 1 {
			t.Fatalf("expected 1 answer, got %d", len(m.Answer))
		}

		txtRecord, ok := m.Answer[0].(*dns.TXT)
		if !ok {
			t.Fatal("expected TXT record")
		}

		if len(txtRecord.Txt) != 1 || txtRecord.Txt[0] != "test-acme-value" {
			t.Errorf("expected TXT value 'test-acme-value', got %v", txtRecord.Txt)
		}
	})

	t.Run("invalid qname", func(t *testing.T) {
		m := new(dns.Msg)
		err := server.handleACMETXTChallenge("invalid.", m)
		if err == nil {
			t.Error("expected error for invalid qname")
		}
	})

	t.Run("no ACME records found", func(t *testing.T) {
		m := new(dns.Msg)
		err := server.handleACMETXTChallenge("_acme-challenge.nonexistent.com.", m)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(m.Answer) != 0 {
			t.Errorf("expected 0 answers, got %d", len(m.Answer))
		}
	})
}

// Mock DNS ResponseWriter for testing
type mockResponseWriter struct {
	remoteAddr net.Addr
	msg        *dns.Msg
}

func (m *mockResponseWriter) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 53}
}

func (m *mockResponseWriter) RemoteAddr() net.Addr {
	return m.remoteAddr
}

func (m *mockResponseWriter) WriteMsg(msg *dns.Msg) error {
	m.msg = msg
	return nil
}

func (m *mockResponseWriter) Write([]byte) (int, error) {
	return 0, nil
}

func (m *mockResponseWriter) Close() error {
	return nil
}

func (m *mockResponseWriter) TsigStatus() error {
	return nil
}

func (m *mockResponseWriter) TsigTimersOnly(bool) {}

func (m *mockResponseWriter) Hijack() {}

func TestServer_Start(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	acmeProvider := acme.NewProvider(slog.Default())
	logger := slog.Default()

	// Use high port to avoid permission issues
	server, err := NewServer("example.com", 15353, manager, acmeProvider, logger, idGen)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errChan := make(chan error, 1)
	go func() {
		errChan <- server.Start(ctx)
	}()

	// Give server time to start
	time.Sleep(200 * time.Millisecond)

	// Test that server is running by making a DNS query
	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion("test.example.com.", dns.TypeA)

	r, _, err := c.Exchange(m, "127.0.0.1:15353")
	if err != nil {
		t.Fatalf("failed to query DNS server: %v", err)
	}

	if len(r.Answer) == 0 {
		t.Error("expected DNS answer, got none")
	}

	// Stop server
	cancel()

	select {
	case err := <-errChan:
		if err != nil && err != context.Canceled {
			t.Errorf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop in time")
	}
}

func TestServer_StartContextCancelled(t *testing.T) {
	idGen := func() string { return "test-id" }
	manager := storage.NewMemoryManager(idGen)
	acmeProvider := acme.NewProvider(slog.Default())
	logger := slog.Default()

	server, err := NewServer("example.com", 15354, manager, acmeProvider, logger, idGen)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	errChan := make(chan error, 1)
	go func() {
		errChan <- server.Start(ctx)
	}()

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	// Cancel context
	cancel()

	select {
	case <-errChan:
		// Server stopped successfully
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop after context cancellation")
	}
}
