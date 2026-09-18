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

// HookIDFromHost returns the hook ID encoded in a host name under domain: the
// first label of the subdomain part. It returns "" when host is not a strict
// subdomain of domain (the apex itself included).
//
// Matching is case-insensitive and a trailing dot is ignored, so a DNS qname
// (whose casing resolvers randomise, per DNS-0x20), a Host header and an SMTP
// recipient domain can all share one rule. Callers strip any port first.
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
