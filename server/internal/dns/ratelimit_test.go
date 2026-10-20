package dns

import (
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/jomar/hookd/internal/storage"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter(perSecond, burst int) (*rateLimiter, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := newRateLimiter(perSecond, burst)
	l.now = clock.now
	return l, clock
}

func udpAddr(ip string) net.Addr {
	return &net.UDPAddr{IP: net.ParseIP(ip), Port: 5353}
}

func TestRateLimiter_BurstThenRefill(t *testing.T) {
	l, clock := newTestLimiter(10, 3)
	src := udpAddr("192.0.2.9")
	for i := range 3 {
		if !l.allow(src) {
			t.Fatalf("reply %d within the burst was denied", i)
		}
	}
	if l.allow(src) {
		t.Fatal("reply beyond the burst was allowed")
	}
	clock.advance(100 * time.Millisecond)
	if !l.allow(src) {
		t.Fatal("refilled token was denied")
	}
	if l.allow(src) {
		t.Fatal("bucket refilled more than the rate")
	}
}

func TestRateLimiter_KeysOnPrefix(t *testing.T) {
	l, _ := newTestLimiter(1, 1)
	if !l.allow(udpAddr("192.0.2.1")) {
		t.Fatal("first reply denied")
	}
	if l.allow(udpAddr("192.0.2.200")) {
		t.Error("same /24 has its own bucket")
	}
	if !l.allow(udpAddr("192.0.3.1")) {
		t.Error("another /24 shares the bucket")
	}
	if !l.allow(udpAddr("::ffff:198.51.100.1")) {
		t.Error("IPv4-mapped source denied")
	}
	if l.allow(udpAddr("198.51.100.2")) {
		t.Error("IPv4-mapped and plain IPv4 use different buckets")
	}
	if !l.allow(udpAddr("2001:db8:0:1::1")) {
		t.Fatal("first IPv6 reply denied")
	}
	if l.allow(udpAddr("2001:db8:0:ff::1")) {
		t.Error("same /56 has its own bucket")
	}
	if !l.allow(udpAddr("2001:db8:0:100::1")) {
		t.Error("another /56 shares the bucket")
	}
}

func TestRateLimiter_IgnoresNonUDP(t *testing.T) {
	l, _ := newTestLimiter(1, 1)
	src := &net.TCPAddr{IP: net.ParseIP("192.0.2.9"), Port: 5353}
	for range 3 {
		if !l.allow(src) {
			t.Fatal("non-UDP source was limited")
		}
	}
}

func TestRateLimiter_FullTableSweepsIdleBuckets(t *testing.T) {
	l, clock := newTestLimiter(10, 10)
	for i := range maxRateBuckets {
		l.buckets[netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}), 32)] = &bucket{last: clock.t}
	}
	clock.advance(time.Second)
	if !l.allow(udpAddr("192.0.2.9")) {
		t.Fatal("refilled buckets were not swept")
	}
	if len(l.buckets) != 1 {
		t.Errorf("table holds %d buckets after the sweep, want 1", len(l.buckets))
	}
}

func TestRateLimiter_FullTableDeniesNewPrefixes(t *testing.T) {
	l, clock := newTestLimiter(10, 10)
	fill := func() {
		for i := range maxRateBuckets {
			l.buckets[netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}), 32)] = &bucket{tokens: 10, last: clock.t}
		}
	}
	fill()
	if l.allow(udpAddr("192.0.2.9")) {
		t.Fatal("new prefix allowed while every bucket is active")
	}
	// The next sweep waits a second, even once buckets are idle.
	clock.advance(999 * time.Millisecond)
	if l.allow(udpAddr("192.0.2.9")) {
		t.Fatal("table swept twice within a second")
	}
}

func TestRateLimiter_ReportsOncePerMinute(t *testing.T) {
	l, clock := newTestLimiter(1, 1)
	src := udpAddr("192.0.2.9")
	l.allow(src)
	l.allow(src)
	l.allow(src)
	if n, ok := l.report(); !ok || n != 2 {
		t.Fatalf("report = %d, %v; want 2, true", n, ok)
	}
	l.allow(src)
	if _, ok := l.report(); ok {
		t.Fatal("reported twice within a minute")
	}
	clock.advance(time.Minute)
	if n, ok := l.report(); !ok || n != 1 {
		t.Fatalf("report = %d, %v; want 1, true", n, ok)
	}
}

func TestServer_RateLimitTruncatesButRecords(t *testing.T) {
	s, manager := newZoneServer(t, ServerOptions{RateLimitPerSecond: 1, RateLimitBurst: 2})
	hook := manager.CreateHook("example.com", storage.CreateOptions{})
	name := fmt.Sprintf("%s.example.com.", hook.ID)

	for i := range 3 {
		m := new(dns.Msg)
		m.SetQuestion(name, dns.TypeA)
		m.SetEdns0(1232, false)
		w := &mockResponseWriter{remoteAddr: udpAddr("192.0.2.9")}
		s.handleDNSRequest(w, m)

		limited := i == 2
		if w.msg.Truncated != limited {
			t.Errorf("query %d: TC = %v, want %v", i, w.msg.Truncated, limited)
		}
		if limited {
			if len(w.msg.Answer)+len(w.msg.Ns) != 0 || len(w.msg.Question) != 1 || w.msg.IsEdns0() == nil {
				t.Errorf("slip reply: %d answers, %d authority, %d questions, OPT=%v",
					len(w.msg.Answer), len(w.msg.Ns), len(w.msg.Question), w.msg.IsEdns0() != nil)
			}
			if w.msg.Len() > m.Len() {
				t.Errorf("slip reply is %d bytes, larger than the %d-byte query", w.msg.Len(), m.Len())
			}
		} else if len(w.msg.Answer) == 0 {
			t.Errorf("query %d: no answer", i)
		}
	}

	if interactions := manager.PollInteractions(hook.ID); len(interactions) != 3 {
		t.Errorf("recorded %d interactions, want 3 including the limited one", len(interactions))
	}
}

func TestServer_RateLimitDisabledByDefault(t *testing.T) {
	s, _ := newZoneServer(t, ServerOptions{})
	for i := range 50 {
		if r := query(s, "abc.example.com.", dns.TypeA); r.Truncated {
			t.Fatalf("query %d truncated without a rate limit", i)
		}
	}
}

func TestServer_RateLimitFallsBackToTCP(t *testing.T) {
	_, addr := startServer(t, ServerOptions{RateLimitPerSecond: 1, RateLimitBurst: 1})

	udp := &dns.Client{Net: "udp", Timeout: time.Second}
	m := new(dns.Msg)
	m.SetQuestion("abc.example.com.", dns.TypeA)
	var r *dns.Msg
	var err error
	for range 3 {
		if r, _, err = udp.Exchange(m, addr); err != nil {
			t.Fatal(err)
		}
		if r.Truncated {
			break
		}
	}
	if !r.Truncated || len(r.Answer) != 0 {
		t.Fatalf("expected a slip reply over UDP, got TC=%v with %d answers", r.Truncated, len(r.Answer))
	}

	tcp := &dns.Client{Net: "tcp", Timeout: time.Second}
	for i := range 3 {
		r, _, err = tcp.Exchange(m, addr)
		if err != nil {
			t.Fatal(err)
		}
		if r.Truncated || len(r.Answer) != 2 {
			t.Fatalf("TCP query %d: TC=%v with %d answers", i, r.Truncated, len(r.Answer))
		}
	}
}
