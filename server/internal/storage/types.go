package storage

import (
	"strconv"
	"time"
	"unicode/utf8"
)

// TruncateBody caps body at max bytes, cutting on a UTF-8 rune boundary, and
// reports whether it cut. A max of zero or less means no limit.
func TruncateBody(body string, max int) (string, bool) {
	if max <= 0 || len(body) <= max {
		return body, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	return body[:cut], true
}

// Hook represents a registered hook
type Hook struct {
	ID        string         `json:"id"`
	DNS       string         `json:"dns"`
	HTTP      string         `json:"http"`
	HTTPS     string         `json:"https"`
	SMTP      string         `json:"smtp,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	ExpiresAt time.Time      `json:"expires_at"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// CreateOptions carries the per-hook parameters supplied at registration time.
// A zero TTL means "no explicit expiry" (the hook is never removed by the
// hook-TTL eviction pass); callers that want a hook to expire must set it.
type CreateOptions struct {
	TTL      time.Duration
	Metadata map[string]any
	// Mirrors server.smtp.enabled: without a listener the address would
	// black-hole, so it is not advertised.
	SMTPEnabled bool
}

// newHook builds a Hook from an id, domain and options. It is shared by every
// storage backend so the DNS/URL layout and the TTL-to-ExpiresAt rule live in
// exactly one place.
func newHook(id, domain string, opts CreateOptions) *Hook {
	now := time.Now().UTC()
	hook := &Hook{
		ID:        id,
		DNS:       id + "." + domain,
		HTTP:      "http://" + id + "." + domain,
		HTTPS:     "https://" + id + "." + domain,
		CreatedAt: now,
		Metadata:  opts.Metadata,
	}
	if opts.SMTPEnabled {
		hook.SMTP = id + "@" + domain
	}
	if opts.TTL > 0 {
		hook.ExpiresAt = now.Add(opts.TTL)
	}
	return hook
}

// InteractionType represents the type of interaction
type InteractionType string

const (
	InteractionTypeDNS  InteractionType = "dns"
	InteractionTypeHTTP InteractionType = "http"
	InteractionTypeSMTP InteractionType = "smtp"
)

// Interaction represents a captured DNS or HTTP interaction
type Interaction struct {
	ID string `json:"id"`
	// Seq is assigned by the store on capture and increases per hook.
	Seq       int64                  `json:"seq"`
	Type      InteractionType        `json:"type"`
	Timestamp time.Time              `json:"timestamp"`
	SourceIP  string                 `json:"source_ip"`
	Data      map[string]interface{} `json:"data"`
}

// MemoryStats represents detailed memory statistics
type MemoryStats struct {
	AllocMB     int    `json:"alloc_mb"`      // Bytes allocated and still in use
	HeapInuseMB int    `json:"heap_inuse_mb"` // Bytes in use by the heap
	SysMB       int    `json:"sys_mb"`        // Total memory obtained from OS
	GCRuns      uint32 `json:"gc_runs"`       // Number of completed GC cycles
}

// PollResult represents the result of polling a single hook
type PollResult struct {
	Interactions []*Interaction `json:"interactions"`
	// Set on cursor reads: the highest seq evicted before being acknowledged.
	DroppedThrough *int64 `json:"dropped_through,omitempty"`
	Error          string `json:"error,omitempty"`
}

// CursorRead is a non-destructive read of a hook's interactions past a cursor.
type CursorRead struct {
	Interactions []*Interaction
	// DroppedThrough is the highest seq evicted before being acknowledged; a
	// value above the caller's cursor means interactions were lost.
	DroppedThrough int64
}

// HookActivity summarises a long-lived hook that has pending interactions. It
// powers the activity endpoint, which lets a client discover which of its many
// long-lived hooks have fired without polling each one individually.
type HookActivity struct {
	Hook              *Hook     `json:"hook"`
	PendingCount      int       `json:"pending_count"`
	LastInteractionAt time.Time `json:"last_interaction_at"`
	LastSeq           int64     `json:"last_seq"`
}

// DNSInteraction creates a DNS interaction
func DNSInteraction(id, sourceIP, qname, qtype string) *Interaction {
	return &Interaction{
		ID:        id,
		Type:      InteractionTypeDNS,
		Timestamp: time.Now().UTC(),
		SourceIP:  sourceIP,
		Data: map[string]interface{}{
			"qname": qname,
			"qtype": qtype,
		},
	}
}

// HTTPInteraction creates an HTTP interaction
func HTTPInteraction(id, sourceIP, method, path string, headers map[string]string, body string) *Interaction {
	return &Interaction{
		ID:        id,
		Type:      InteractionTypeHTTP,
		Timestamp: time.Now().UTC(),
		SourceIP:  sourceIP,
		Data: map[string]interface{}{
			"method":  method,
			"path":    path,
			"headers": headers,
			"body":    body,
		},
	}
}

// SMTPInteraction creates an SMTP interaction. body is the raw RFC 5322
// message, stored under "body" so the long-lived store truncates it like an
// HTTP body. tag is the recipient's sub-address suffix.
func SMTPInteraction(id, sourceIP, helo, mailFrom, rcptTo, tag, subject, body string) *Interaction {
	return &Interaction{
		ID:        id,
		Type:      InteractionTypeSMTP,
		Timestamp: time.Now().UTC(),
		SourceIP:  sourceIP,
		Data: map[string]interface{}{
			"helo":      helo,
			"mail_from": mailFrom,
			"rcpt_to":   rcptTo,
			"tag":       tag,
			"subject":   subject,
			"body":      body,
		},
	}
}

// MatchesMetadata reports whether every filter key is a top-level metadata key
// whose scalar value, rendered as text, equals the filter value.
func MatchesMetadata(metadata map[string]any, filter map[string]string) bool {
	for key, want := range filter {
		var got string
		switch v := metadata[key].(type) {
		case string:
			got = v
		case float64:
			got = strconv.FormatFloat(v, 'f', -1, 64)
		case bool:
			got = strconv.FormatBool(v)
		default:
			return false
		}
		if got != want {
			return false
		}
	}
	return true
}
