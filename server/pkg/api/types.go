package api

import "time"

// Hook represents a registered hook (public API type)
type Hook struct {
	ID        string         `json:"id"`
	DNS       string         `json:"dns"`
	HTTP      string         `json:"http"`
	HTTPS     string         `json:"https"`
	CreatedAt time.Time      `json:"created_at"`
	ExpiresAt time.Time      `json:"expires_at"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// Interaction represents a captured interaction (public API type)
type Interaction struct {
	ID        string                 `json:"id"`
	Type      string                 `json:"type"`
	Timestamp time.Time              `json:"timestamp"`
	SourceIP  string                 `json:"source_ip"`
	Data      map[string]interface{} `json:"data"`
}

// PollResponse represents the response from /poll/:id
type PollResponse struct {
	Interactions []Interaction  `json:"interactions"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// ErrorResponse represents an error response
type ErrorResponse struct {
	Error string `json:"error"`
}

// RegisterRequest represents the request body for /register. TTL is optional: a
// value above the ephemeral hook TTL (e.g. "168h" or "7d") registers a durable
// long-lived hook; omit it for an ephemeral hook. Metadata is stored with the
// hook and echoed back when it is polled.
type RegisterRequest struct {
	Count    int            `json:"count,omitempty"`
	TTL      string         `json:"ttl,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// HookActivity summarises a long-lived hook that has pending interactions.
type HookActivity struct {
	Hook              Hook      `json:"hook"`
	PendingCount      int       `json:"pending_count"`
	LastInteractionAt time.Time `json:"last_interaction_at"`
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
	CreatedAt time.Time `json:"created_at,omitempty"`

	// Multiple hooks response (when count>1)
	Hooks []Hook `json:"hooks,omitempty"`
}
