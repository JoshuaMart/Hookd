package hookd

// Hook represents a registered hook with its endpoints. SMTP is set only when
// the server runs a mail listener. ExpiresAt and Metadata are populated for
// long-lived hooks (registered with a TTL) and left empty otherwise.
type Hook struct {
	ID        string         `json:"id"`
	DNS       string         `json:"dns"`
	HTTP      string         `json:"http"`
	HTTPS     string         `json:"https"`
	SMTP      string         `json:"smtp,omitempty"`
	CreatedAt string         `json:"created_at"`
	ExpiresAt string         `json:"expires_at,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// RegisterOptions configures hook registration. A TTL above the server's
// ephemeral hook_ttl (e.g. "168h" or "7d") registers a durable long-lived hook
// that survives restarts; omit it for an ephemeral hook. Metadata is stored
// with the hook and echoed back when it is polled.
type RegisterOptions struct {
	Count    int
	TTL      string
	Metadata map[string]any
}

// HookSpec describes one hook of a RegisterBatch call.
type HookSpec struct {
	TTL      string         `json:"ttl,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// HookActivity summarises a long-lived hook that has pending interactions.
type HookActivity struct {
	Hook              Hook   `json:"hook"`
	PendingCount      int    `json:"pending_count"`
	LastInteractionAt string `json:"last_interaction_at"`
	// LastSeq is the newest seq held: at or below a cursor, nothing is new.
	LastSeq int64 `json:"last_seq"`
}

// Interaction represents a DNS, HTTP or SMTP interaction captured by a hook.
type Interaction struct {
	ID string `json:"id"`
	// Seq increases per hook; it is the cursor for Read and Ack.
	Seq       int64          `json:"seq"`
	Type      string         `json:"type"`
	Timestamp string         `json:"timestamp"`
	SourceIP  string         `json:"source_ip"`
	Data      map[string]any `json:"data"`
}

// IsDNS returns true if the interaction is a DNS interaction.
func (i *Interaction) IsDNS() bool { return i.Type == "dns" }

// IsHTTP returns true if the interaction is an HTTP interaction.
func (i *Interaction) IsHTTP() bool { return i.Type == "http" }

// IsSMTP returns true if the interaction is an SMTP interaction.
func (i *Interaction) IsSMTP() bool { return i.Type == "smtp" }

// BatchResult holds the poll result for a single hook in a batch poll.
// DroppedThrough is set by ReadBatch only.
type BatchResult struct {
	Interactions   []Interaction
	DroppedThrough int64
	Error          string
}

// CursorRead is the result of a non-destructive Read.
type CursorRead struct {
	Interactions []Interaction
	// DroppedThrough is the highest seq the server evicted before it was
	// acknowledged; see Lost.
	DroppedThrough int64
	Metadata       map[string]any
}

// Lost reports whether interactions past the cursor were evicted unread.
func (r *CursorRead) Lost(after int64) bool { return r.DroppedThrough > after }

// AckResult holds the outcome of acknowledging a single hook in AckBatch.
type AckResult struct {
	Acknowledged int
	Error        string
}

// Metrics holds server metrics data.
type Metrics map[string]any
