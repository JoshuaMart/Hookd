// Package netutil holds small networking helpers shared across servers.
package netutil

import (
	"net"
	"strings"
)

// ExtractIP returns the host portion of a "host:port" remote address. It
// handles IPv6 addresses (e.g. "[::1]:8080" -> "::1") and falls back to the
// input unchanged when there is no port.
func ExtractIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// HookIDFromHost returns the first label of host's subdomain part under domain,
// or "" when host is not a strict subdomain of it. Matching ignores case
// (DNS-0x20) and a trailing dot; callers strip any port first.
func HookIDFromHost(host, domain string) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))

	suffix := "." + strings.ToLower(strings.TrimSuffix(domain, "."))
	if !strings.HasSuffix(host, suffix) {
		return ""
	}

	subdomain := strings.TrimSuffix(host, suffix)

	// Handle multi-level subdomains (take the first label). strings.Split
	// always returns at least one element, so parts[0] is safe.
	return strings.Split(subdomain, ".")[0]
}

// HookIDFromHostTail returns the label immediately before domain. A caller
// may prepend its own DNS labels to a registered hook, for example
// allowed.example.<hook-id>.hookd.example; the registered ID is then the tail.
// Callers must verify that the returned ID exists before storing an event.
func HookIDFromHostTail(host, domain string) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	suffix := "." + strings.ToLower(strings.TrimSuffix(domain, "."))
	if !strings.HasSuffix(host, suffix) {
		return ""
	}
	subdomain := strings.TrimSuffix(host, suffix)
	parts := strings.Split(subdomain, ".")
	return parts[len(parts)-1]
}

// ResolveHookID picks the hook a host belongs to: the first label when it is
// registered, else the tail label when that one is, else the first label so
// callers keep their usual handling of unknown hooks.
func ResolveHookID(host, domain string, has func(string) bool) string {
	id := HookIDFromHost(host, domain)
	if has(id) {
		return id
	}
	if tail := HookIDFromHostTail(host, domain); has(tail) {
		return tail
	}
	return id
}
