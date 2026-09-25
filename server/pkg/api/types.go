package api

import "time"

// Hook represents a registered hook (public API type)
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

// Interaction represents a captured DNS, HTTP or SMTP interaction (public API
// type)
type Interaction struct {
	ID        string                 `json:"id"`
	Seq       int64                  `json:"seq"`
	Type      string                 `json:"type"`
	Timestamp time.Time              `json:"timestamp"`
	SourceIP  string                 `json:"source_ip"`
	Data      map[string]interface{} `json:"data"`
}

// PollResponse represents the response from /poll/:id. DroppedThrough is set
// on ?after= reads: above the cursor, interactions were evicted unread.
type PollResponse struct {
	Interactions   []Interaction  `json:"interactions"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	DroppedThrough *int64         `json:"dropped_through,omitempty"`
}

// AckResponse represents the response from DELETE /poll/:id?through=
type AckResponse struct {
	Acknowledged int `json:"acknowledged"`
}

// ErrorResponse represents an error response
type ErrorResponse struct {
	Error string `json:"error"`
}

// RegisterRequest represents the request body for /register. TTL is optional: a
// value above the ephemeral hook TTL (e.g. "168h" or "7d") registers a durable
// long-lived hook; omit it for an ephemeral hook. Metadata is stored with the
// hook and echoed back when it is polled. Hooks registers one hook per entry,
// each with its own ttl and metadata; it excludes the other fields.
type RegisterRequest struct {
	Count    int            `json:"count,omitempty"`
	TTL      string         `json:"ttl,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Hooks    []HookSpec     `json:"hooks,omitempty"`
}

// HookSpec is one entry of RegisterRequest.Hooks.
type HookSpec struct {
	TTL      string         `json:"ttl,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// HooksResponse represents the response from /hooks.
type HooksResponse struct {
	Hooks []Hook `json:"hooks"`
}

// HookActivity summarises a long-lived hook that has pending interactions.
type HookActivity struct {
	Hook              Hook      `json:"hook"`
	PendingCount      int       `json:"pending_count"`
	LastInteractionAt time.Time `json:"last_interaction_at"`
	LastSeq           int64     `json:"last_seq"`
}

// ActivityResponse represents the response from /activity.
type ActivityResponse struct {
	Hooks []HookActivity `json:"hooks"`
}

// RegisterResponse represents the response from /register
// For single hook (count=1 or not specified), returns Hook directly
// For multiple hooks (count>1), returns Hooks array
type RegisterResponse struct {
	// Single hook response fields (when count=1 or omitted)
	ID        string    `json:"id,omitempty"`
	DNS       string    `json:"dns,omitempty"`
	HTTP      string    `json:"http,omitempty"`
	HTTPS     string    `json:"https,omitempty"`
	SMTP      string    `json:"smtp,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`

	// Multiple hooks response (when count>1)
	Hooks []Hook `json:"hooks,omitempty"`
}
