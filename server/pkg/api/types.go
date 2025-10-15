package api

import "time"

// Hook represents a registered hook (public API type)
type Hook struct {
	ID        string    `json:"id"`
	DNS       string    `json:"dns"`
	HTTP      string    `json:"http"`
	HTTPS     string    `json:"https"`
	CreatedAt time.Time `json:"created_at"`
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
	Interactions []Interaction `json:"interactions"`
}

// ErrorResponse represents an error response
type ErrorResponse struct {
	Error string `json:"error"`
}
