package smtp

import (
	"strings"

	"github.com/jomar/hookd/internal/netutil"
)

// hookIDFromRecipient splits a RCPT TO into its hook ID and sub-address tag.
// ok is false only when the address is outside our domain — the one case the
// server refuses. Layouts: <hookid>[+tag]@domain, or the subdomain fallback
// <anything>[+tag]@<hookid>.domain.
func hookIDFromRecipient(addr, domain string) (id, tag string, ok bool) {
	addr = strings.TrimSpace(addr)
	addr = strings.TrimPrefix(addr, "<")
	addr = strings.TrimSuffix(addr, ">")
	addr = strings.ToLower(strings.TrimSpace(addr))

	if addr == "" {
		return "", "", false
	}

	// Strip a source route (RFC 5321 4.1.2): only the final mailbox matters.
	// With no colon there is no route, just an empty local part.
	if strings.HasPrefix(addr, "@") {
		if _, rest, found := strings.Cut(addr, ":"); found {
			addr = rest
		}
	}

	// RFC 5321 4.5.1 requires bare <postmaster>; it matches no hook and is
	// dropped like any other unmatched recipient.
	if addr == "postmaster" {
		return "postmaster", "", true
	}

	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return "", "", false
	}
	local, rcptDomain := addr[:at], addr[at+1:]
	rcptDomain = strings.TrimSuffix(rcptDomain, ".")

	// Sub-addressing: everything after the first '+' is a label, not routing.
	localID, tag, _ := strings.Cut(local, "+")

	if rcptDomain == strings.ToLower(strings.TrimSuffix(domain, ".")) {
		return localID, tag, true
	}

	// Subdomain fallback, sharing the extraction rule with DNS and HTTP.
	if sub := netutil.HookIDFromHost(rcptDomain, domain); sub != "" {
		return sub, tag, true
	}

	return "", "", false
}
