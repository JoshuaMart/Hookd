package dns

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/libdns/libdns"
	"github.com/miekg/dns"

	"github.com/jomar/hookd/internal/acme"
	"github.com/jomar/hookd/internal/storage"
)

var testNameservers = []Nameserver{
	{Name: "ns1.example.com", IP: "203.0.113.1"},
	{Name: "NS2.example.com.", IP: "203.0.113.2"},
}

func newZoneServer(t *testing.T, opts ServerOptions) (*Server, *storage.MemoryManager) {
	t.Helper()
	idGen := opts.IDGenerator
	if idGen == nil {
		idGen = func() string { return "test-id" }
	}
	manager := storage.NewMemoryManager(idGen)
	opts.Domain = "example.com"
	if opts.PublicIPs == nil {
		opts.PublicIPs = []string{"198.51.100.1", "198.51.100.2"}
	}
	opts.Storage = manager
	if opts.ACMEProvider == nil {
		opts.ACMEProvider = acme.NewProvider(slog.Default())
	}
	opts.Logger = slog.Default()
	opts.IDGenerator = idGen
	server, err := NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	return server, manager
}

func query(s *Server, name string, qtype uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	w := &mockResponseWriter{remoteAddr: &net.UDPAddr{IP: net.ParseIP("192.0.2.9"), Port: 5353}}
	s.handleDNSRequest(w, m)
	return w.msg
}

func aAddrs(rrs []dns.RR) []string {
	var out []string
	for _, rr := range rrs {
		if a, ok := rr.(*dns.A); ok {
			out = append(out, a.A.String())
		}
	}
	return out
}

func TestNewServer_RejectsBadAddresses(t *testing.T) {
	base := ServerOptions{Domain: "example.com", Storage: storage.NewMemoryManager(nil), Logger: slog.Default()}
	for name, mod := range map[string]func(*ServerOptions){
		"IPv6 public IP":         func(o *ServerOptions) { o.PublicIPs = []string{"2001:db8::1"} },
		"invalid public IP":      func(o *ServerOptions) { o.PublicIPs = []string{"nope"} },
		"name server off domain": func(o *ServerOptions) { o.Nameservers = []Nameserver{{Name: "ns1.other.com", IP: "203.0.113.1"}} },
		"name server bad IP":     func(o *ServerOptions) { o.Nameservers = []Nameserver{{Name: "ns1.example.com", IP: "x"}} },
	} {
		t.Run(name, func(t *testing.T) {
			opts := base
			opts.PublicIPs = []string{"198.51.100.1"}
			mod(&opts)
			if _, err := NewServer(opts); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestServer_AReturnsEveryPublicIP(t *testing.T) {
	s, manager := newZoneServer(t, ServerOptions{})
	hook := manager.CreateHook("example.com", storage.CreateOptions{})

	r := query(s, hook.ID+".example.com.", dns.TypeA)
	if got := aAddrs(r.Answer); len(got) != 2 || got[0] != "198.51.100.1" || got[1] != "198.51.100.2" {
		t.Fatalf("A answers = %v, want both public IPs", got)
	}
	if len(r.Ns) != 0 {
		t.Errorf("a positive answer must not carry an SOA, got %v", r.Ns)
	}
	if n := len(manager.PollInteractions(hook.ID)); n != 1 {
		t.Errorf("expected one interaction, got %d", n)
	}
	if got := aAddrs(query(s, "example.com.", dns.TypeA).Answer); len(got) != 2 {
		t.Errorf("apex A answers = %v, want both public IPs", got)
	}
}

func TestServer_NSMatchesGlue(t *testing.T) {
	s, _ := newZoneServer(t, ServerOptions{Nameservers: testNameservers})

	r := query(s, "ExAmPlE.com.", dns.TypeNS)
	if len(r.Answer) != 2 {
		t.Fatalf("expected 2 NS answers, got %v", r.Answer)
	}
	for i, want := range []string{"ns1.example.com.", "ns2.example.com."} {
		ns := r.Answer[i].(*dns.NS)
		if ns.Ns != want || ns.Hdr.Name != "ExAmPlE.com." {
			t.Errorf("NS[%d] = %s %s, want %s", i, ns.Hdr.Name, ns.Ns, want)
		}
	}
	glue := map[string]string{}
	for _, rr := range r.Extra {
		if a, ok := rr.(*dns.A); ok {
			glue[a.Hdr.Name] = a.A.String()
		}
	}
	if glue["ns1.example.com."] != "203.0.113.1" || glue["ns2.example.com."] != "203.0.113.2" {
		t.Errorf("additional glue = %v", glue)
	}

	for name, want := range map[string]string{"ns1.example.com.": "203.0.113.1", "Ns2.Example.Com.": "203.0.113.2"} {
		if got := aAddrs(query(s, name, dns.TypeA).Answer); len(got) != 1 || got[0] != want {
			t.Errorf("A %s = %v, want only its glue %s", name, got, want)
		}
	}
}

func TestServer_NSOnNonApexIsNODATA(t *testing.T) {
	s, _ := newZoneServer(t, ServerOptions{Nameservers: testNameservers})
	r := query(s, "abc.example.com.", dns.TypeNS)
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 || len(r.Ns) != 1 {
		t.Fatalf("expected NODATA with an SOA, got %v", r)
	}
}

func TestServer_NameserverLabelsAreNotHooks(t *testing.T) {
	s, manager := newZoneServer(t, ServerOptions{
		Nameservers: testNameservers,
		IDGenerator: func() string { return "ns1" },
	})
	manager.CreateHook("example.com", storage.CreateOptions{})

	for _, name := range []string{"ns1.example.com.", "NS1.example.com."} {
		for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeTXT} {
			query(s, name, qtype)
		}
	}
	if n := manager.Stats().InteractionsTotal; n != 0 {
		t.Errorf("recorded %d interactions for name server labels, want 0", n)
	}
}

func TestServer_SOA(t *testing.T) {
	t.Run("apex with name servers", func(t *testing.T) {
		s, _ := newZoneServer(t, ServerOptions{Nameservers: testNameservers, SOAMinimumTTL: 5 * time.Minute})
		r := query(s, "example.com.", dns.TypeSOA)
		if len(r.Answer) != 1 {
			t.Fatalf("expected one SOA answer, got %v", r.Answer)
		}
		soa := r.Answer[0].(*dns.SOA)
		if soa.Ns != "ns1.example.com." || soa.Mbox != "hostmaster.example.com." {
			t.Errorf("MNAME/RNAME = %s %s", soa.Ns, soa.Mbox)
		}
		if soa.Minttl != 300 || soa.Hdr.Ttl != 300 || soa.Serial != soaSerial {
			t.Errorf("minimum/ttl/serial = %d/%d/%d", soa.Minttl, soa.Hdr.Ttl, soa.Serial)
		}
		if len(r.Ns) != 0 {
			t.Errorf("an SOA answer needs no authority SOA, got %v", r.Ns)
		}
	})
	t.Run("apex without name servers", func(t *testing.T) {
		s, _ := newZoneServer(t, ServerOptions{})
		soa := query(s, "example.com.", dns.TypeSOA).Answer[0].(*dns.SOA)
		if soa.Ns != "example.com." || soa.Minttl != 60 {
			t.Errorf("MNAME/minimum = %s/%d, want the apex and 60", soa.Ns, soa.Minttl)
		}
	})
	t.Run("non-apex is NODATA", func(t *testing.T) {
		s, _ := newZoneServer(t, ServerOptions{})
		r := query(s, "abc.example.com.", dns.TypeSOA)
		if len(r.Answer) != 0 || len(r.Ns) != 1 {
			t.Fatalf("expected NODATA with an SOA, got %v", r)
		}
	})
}

func TestServer_NODATACarriesSOA(t *testing.T) {
	s, _ := newZoneServer(t, ServerOptions{Nameservers: testNameservers})
	for _, qtype := range []uint16{dns.TypeAAAA, dns.TypeCAA, dns.TypeHTTPS} {
		r := query(s, "abc.example.com.", qtype)
		if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 || len(r.Ns) != 1 {
			t.Fatalf("%s: expected NODATA with one SOA, got %v", dns.TypeToString[qtype], r)
		}
		soa, ok := r.Ns[0].(*dns.SOA)
		if !ok || soa.Hdr.Name != "example.com." || soa.Ns != "ns1.example.com." {
			t.Errorf("%s: authority = %v", dns.TypeToString[qtype], r.Ns)
		}
	}

	if r := query(s, "other.com.", dns.TypeAAAA); len(r.Ns) != 0 {
		t.Errorf("an out-of-zone query must not carry our SOA, got %v", r.Ns)
	}
}

func TestServer_ACMENODATAHasNoSOA(t *testing.T) {
	s, _ := newZoneServer(t, ServerOptions{Nameservers: testNameservers})
	r := query(s, "_acme-challenge.example.com.", dns.TypeTXT)
	if len(r.Answer) != 0 || len(r.Ns) != 0 {
		t.Fatalf("a missing challenge must not be negatively cacheable, got %v", r)
	}
}

func TestServer_MXUnchanged(t *testing.T) {
	s, _ := newZoneServer(t, ServerOptions{Nameservers: testNameservers})
	r := query(s, "abc.example.com.", dns.TypeMX)
	if len(r.Answer) != 1 || r.Answer[0].(*dns.MX).Mx != "example.com." || len(r.Ns) != 0 {
		t.Fatalf("MX = %v", r)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	for range 20 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		pc, err := net.ListenPacket("udp", ln.Addr().String())
		_ = ln.Close()
		if err == nil {
			_ = pc.Close()
			return port
		}
	}
	t.Fatal("no port free on both UDP and TCP")
	return 0
}

// startServer serves on 127.0.0.1 and stops the server, checking a clean shutdown, at cleanup.
func startServer(t *testing.T, opts ServerOptions) (*Server, string) {
	t.Helper()
	port := freePort(t)
	opts.Port = port
	opts.BindAddress = "127.0.0.1"
	s, _ := newZoneServer(t, opts)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for _, network := range []string{"udp", "tcp"} {
		c := &dns.Client{Net: network, Timeout: 200 * time.Millisecond}
		deadline := time.Now().Add(3 * time.Second)
		for {
			m := new(dns.Msg)
			m.SetQuestion("example.com.", dns.TypeSOA)
			if _, _, err := c.Exchange(m, addr); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s listener not ready", network)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return s, addr
}

func TestServer_ServesUDPAndTCP(t *testing.T) {
	provider := acme.NewProvider(slog.Default())
	_, addr := startServer(t, ServerOptions{ACMEProvider: provider})
	if _, err := provider.AppendRecords(context.Background(), "example.com.", []libdns.Record{
		libdns.RR{Type: "TXT", Name: "_acme-challenge", Data: "token-value", TTL: time.Minute},
	}); err != nil {
		t.Fatal(err)
	}

	for _, network := range []string{"udp", "tcp"} {
		t.Run(network, func(t *testing.T) {
			c := &dns.Client{Net: network, Timeout: time.Second}

			m := new(dns.Msg)
			m.SetQuestion("abc.example.com.", dns.TypeA)
			r, _, err := c.Exchange(m, addr)
			if err != nil {
				t.Fatal(err)
			}
			if got := aAddrs(r.Answer); len(got) != 2 {
				t.Errorf("A answers = %v", got)
			}

			m = new(dns.Msg)
			m.SetQuestion("_acme-challenge.example.com.", dns.TypeTXT)
			r, _, err = c.Exchange(m, addr)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Answer) != 1 || r.Answer[0].(*dns.TXT).Txt[0] != "token-value" {
				t.Errorf("ACME TXT = %v", r.Answer)
			}
		})
	}
}

func TestServer_EchoesEDNS0OnBothTransports(t *testing.T) {
	_, addr := startServer(t, ServerOptions{})
	for _, network := range []string{"udp", "tcp"} {
		m := new(dns.Msg)
		m.SetQuestion("abc.example.com.", dns.TypeA)
		m.SetEdns0(4096, false)
		r, _, err := (&dns.Client{Net: network, Timeout: time.Second}).Exchange(m, addr)
		if err != nil {
			t.Fatal(err)
		}
		if r.IsEdns0() == nil {
			t.Errorf("%s: reply to an EDNS0 query has no OPT", network)
		}
	}
}

func TestServer_CapsTCPConnections(t *testing.T) {
	_, addr := startServer(t, ServerOptions{MaxTCPConnections: 1})

	// startServer's readiness probes have closed their connections, but the
	// server releases the slot asynchronously.
	var held net.Conn
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conn := &dns.Conn{Conn: c}
		m := new(dns.Msg)
		m.SetQuestion("example.com.", dns.TypeSOA)
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		if err := conn.WriteMsg(m); err == nil {
			if _, err := conn.ReadMsg(); err == nil {
				held = c
				break
			}
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Fatal("no TCP slot became free")
		}
		time.Sleep(20 * time.Millisecond)
	}

	extra, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = extra.SetReadDeadline(time.Now().Add(time.Second))
	_, err = extra.Read(make([]byte, 1))
	if ne, ok := err.(net.Error); err == nil || ok && ne.Timeout() {
		t.Errorf("a connection over the cap must be closed, got %v", err)
	}
	_ = extra.Close()

	_ = held.Close()
	c := &dns.Client{Net: "tcp", Timeout: time.Second}
	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeSOA)
	deadline = time.Now().Add(2 * time.Second)
	for {
		if _, _, err := c.Exchange(m, addr); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the slot was not released after the held connection closed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestServer_TruncatesUDPAndServesFullOverTCP(t *testing.T) {
	ips := make([]string, 40)
	for i := range ips {
		ips[i] = fmt.Sprintf("198.51.100.%d", i+1)
	}
	_, addr := startServer(t, ServerOptions{PublicIPs: ips})

	m := new(dns.Msg)
	m.SetQuestion("abc.example.com.", dns.TypeA)

	udp := &dns.Client{Net: "udp", Timeout: time.Second}
	r, _, err := udp.Exchange(m, addr)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || len(r.Answer) >= len(ips) {
		t.Errorf("expected a truncated UDP answer, got TC=%v with %d records", r.Truncated, len(r.Answer))
	}

	m.SetEdns0(4096, false)
	r, _, err = udp.Exchange(m, addr)
	if err != nil {
		t.Fatal(err)
	}
	if r.Truncated || len(r.Answer) != len(ips) || r.IsEdns0() == nil {
		t.Errorf("EDNS0 answer: TC=%v, %d records, OPT=%v", r.Truncated, len(r.Answer), r.IsEdns0() != nil)
	}

	tcp := &dns.Client{Net: "tcp", Timeout: time.Second}
	m = new(dns.Msg)
	m.SetQuestion("abc.example.com.", dns.TypeA)
	r, _, err = tcp.Exchange(m, addr)
	if err != nil {
		t.Fatal(err)
	}
	if r.Truncated || len(r.Answer) != len(ips) {
		t.Errorf("TCP answer: TC=%v, %d records", r.Truncated, len(r.Answer))
	}
}
