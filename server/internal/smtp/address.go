package smtp

import (
	"strings"

	"github.com/jomar/hookd/internal/netutil"
)

// hookIDFromRecipient splits a RCPT TO address into its hook ID and optional
// sub-address tag. ok is false when the address is not under our domain, which
// is the only case the server refuses — every in-domain address is accepted,
// whether or not a hook answers to it, so the server is not an enumeration
// oracle and so a signup form probing the address never sees a failure.
//
// Two layouts are understood:
//
//	<hookid>[+tag]@domain          the primary form
//	<anything>[+tag]@<hookid>.domain   the subdomain fallback
func hookIDFromRecipient(addr, domain string) (id, tag string, ok bool) {
	addr = strings.TrimSpace(addr)
	addr = strings.TrimPrefix(addr, "<")
	addr = strings.TrimSuffix(addr, ">")
	addr = strings.ToLower(strings.TrimSpace(addr))

	if addr == "" {
		return "", "", false
	}

	// A source route (RFC 5321 4.1.2, "@hosta,@hostb:user@final") is stripped:
	// the relay hops are not ours to honour, only the final mailbox matters.
	// Without the colon there is no route, just an address whose local part is
	// empty, which the ordinary parsing below handles.
	if strings.HasPrefix(addr, "@") {
		if _, rest, found := strings.Cut(addr, ":"); found {
			addr = rest
		}
	}

	// RFC 5321 4.5.1 requires <postmaster> with no domain to be accepted. It
	// matches no hook, so the message is read and dropped like any other
	// unmatched recipient.
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
