package storage

import (
	"time"
)

// Hook represents a registered hook
type Hook struct {
	ID        string         `json:"id"`
	DNS       string         `json:"dns"`
	HTTP      string         `json:"http"`
	HTTPS     string         `json:"https"`
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
}

// InteractionType represents the type of interaction
type InteractionType string

const (
	InteractionTypeDNS  InteractionType = "dns"
	InteractionTypeHTTP InteractionType = "http"
)

// Interaction represents a captured DNS or HTTP interaction
type Interaction struct {
	ID        string                 `json:"id"`
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
	Error        string         `json:"error,omitempty"`
}

// HookActivity summarises a long-lived hook that has pending interactions. It
// powers the activity endpoint, which lets a client discover which of its many
// long-lived hooks have fired without polling each one individually.
type HookActivity struct {
	Hook              *Hook     `json:"hook"`
	PendingCount      int       `json:"pending_count"`
	LastInteractionAt time.Time `json:"last_interaction_at"`
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
