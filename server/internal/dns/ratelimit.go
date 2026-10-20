package dns

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// maxRateBuckets bounds the limiter's memory when spoofed sources flood it.
const maxRateBuckets = 65536

// rateLimiter is a token bucket per source prefix (/24 IPv4, /56 IPv6) for
// response rate limiting (RRL) of UDP replies.
type rateLimiter struct {
	mu        sync.Mutex
	rate      float64 // tokens per second
	burst     float64
	buckets   map[netip.Prefix]*bucket
	lastSweep time.Time
	denied    int // since the last report
	reported  time.Time
	now       func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perSecond, burst int) *rateLimiter {
	return &rateLimiter{
		rate:    float64(perSecond),
		burst:   float64(burst),
		buckets: make(map[netip.Prefix]*bucket),
		now:     time.Now,
	}
}

// allow spends one token of the source's prefix. A full table denies new
// prefixes: they get a truncated reply and retry over TCP, which is unlimited.
func (l *rateLimiter) allow(addr net.Addr) bool {
	key, ok := sourcePrefix(addr)
	if !ok {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, found := l.buckets[key]
	if !found {
		if len(l.buckets) >= maxRateBuckets && !l.sweep(now) {
			l.denied++
			return false
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		l.denied++
		return false
	}
	b.tokens--
	return true
}

// report returns the replies denied since the last report, at most once a minute.
func (l *rateLimiter) report() (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.denied == 0 || now.Sub(l.reported) < time.Minute {
		return 0, false
	}
	n := l.denied
	l.denied, l.reported = 0, now
	return n, true
}

// sweep drops buckets that have refilled, at most once a second so a flood
// does not scan the table on every packet. It reports whether room was made.
func (l *rateLimiter) sweep(now time.Time) bool {
	if now.Sub(l.lastSweep) < time.Second {
		return false
	}
	l.lastSweep = now
	refill := time.Duration(l.burst / l.rate * float64(time.Second))
	for key, b := range l.buckets {
		if now.Sub(b.last) >= refill {
			delete(l.buckets, key)
		}
	}
	return len(l.buckets) < maxRateBuckets
}

func sourcePrefix(addr net.Addr) (netip.Prefix, bool) {
	udp, ok := addr.(*net.UDPAddr)
	if !ok {
		return netip.Prefix{}, false
	}
	ip, ok := netip.AddrFromSlice(udp.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ip = ip.Unmap()
	bits := 56
	if ip.Is4() {
		bits = 24
	}
	p, err := ip.Prefix(bits)
	return p, err == nil
}
