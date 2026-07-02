// Package netutil holds small networking helpers shared across servers.
package netutil

import "net"

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
