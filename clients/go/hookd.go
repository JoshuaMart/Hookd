// Package hookd provides a Go client for the Hookd interaction logging server.
package hookd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const Version = "1.1.0"

// DefaultMaxResponseBytes bounds how much of a response body the client will
// buffer. A poll can legitimately return many interactions carrying full request
// bodies, so the default is generous; it exists to stop a misdirected or hostile
// endpoint from driving the calling process out of memory.
const DefaultMaxResponseBytes int64 = 64 << 20 // 64 MiB

// Client communicates with a Hookd server.
type Client struct {
	server           string
	token            string
	httpClient       *http.Client
	maxResponseBytes int64
}

// Hook represents a registered hook with its endpoints. ExpiresAt and Metadata
// are populated for long-lived hooks (registered with a TTL) and left empty
// otherwise.
type Hook struct {
	ID        string         `json:"id"`
	DNS       string         `json:"dns"`
	HTTP      string         `json:"http"`
	HTTPS     string         `json:"https"`
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

// HookActivity summarises a long-lived hook that has pending interactions.
type HookActivity struct {
	Hook              Hook   `json:"hook"`
	PendingCount      int    `json:"pending_count"`
	LastInteractionAt string `json:"last_interaction_at"`
}

// Interaction represents a DNS or HTTP interaction captured by a hook.
type Interaction struct {
	Type      string         `json:"type"`
	Timestamp string         `json:"timestamp"`
	SourceIP  string         `json:"source_ip"`
	Data      map[string]any `json:"data"`
}

// IsDNS returns true if the interaction is a DNS interaction.
func (i *Interaction) IsDNS() bool { return i.Type == "dns" }

// IsHTTP returns true if the interaction is an HTTP interaction.
func (i *Interaction) IsHTTP() bool { return i.Type == "http" }

// BatchResult holds the poll result for a single hook in a batch poll.
type BatchResult struct {
	Interactions []Interaction
	Error        string
}

// Metrics holds server metrics data.
type Metrics map[string]any

// Error types

// Error is the base error type for Hookd client errors.
type Error struct {
	Message    string
	StatusCode int
}

func (e *Error) Error() string { return e.Message }

// AuthenticationError indicates a 401 response.
type AuthenticationError struct {
	Message    string
	StatusCode int
}

func (e *AuthenticationError) Error() string { return e.Message }

// NotFoundError indicates a 404 response.
type NotFoundError struct {
	Message    string
	StatusCode int
}

func (e *NotFoundError) Error() string { return e.Message }

// ServerError indicates a 5xx response.
type ServerError struct {
	Message    string
	StatusCode int
}

func (e *ServerError) Error() string { return e.Message }

// ConnectionError indicates a network or timeout failure.
type ConnectionError struct {
	Message    string
	StatusCode int
}

func (e *ConnectionError) Error() string { return e.Message }

// ResponseTooLargeError indicates the server returned more data than the client
// is willing to buffer. Raise the ceiling with SetMaxResponseBytes.
type ResponseTooLargeError struct {
	Message string
	Limit   int64
}

func (e *ResponseTooLargeError) Error() string { return e.Message }

// NewClient creates a new Hookd client.
func NewClient(server, token string) *Client {
	return &Client{
		server: server,
		token:  token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				ResponseHeaderTimeout: 10 * time.Second,
			},
		},
		maxResponseBytes: DefaultMaxResponseBytes,
	}
}

// SetMaxResponseBytes overrides how much of a response body the client buffers.
// A value of zero or less disables the bound.
func (c *Client) SetMaxResponseBytes(n int64) {
	c.maxResponseBytes = n
}

// Register creates one or more ephemeral hooks on the server.
// If count is 0 or 1, a single Hook is returned.
// If count > 1, a slice of Hook is returned.
func (c *Client) Register(count int) ([]Hook, error) {
	return c.RegisterHooks(RegisterOptions{Count: count})
}

// RegisterHooks creates one or more hooks with the given options, allowing a TTL
// (for long-lived hooks) and metadata to be attached.
func (c *Client) RegisterHooks(opts RegisterOptions) ([]Hook, error) {
	if opts.Count < 0 {
		return nil, fmt.Errorf("count must be a positive integer")
	}

	fields := map[string]any{}
	if opts.Count > 1 {
		fields["count"] = opts.Count
	}
	if opts.TTL != "" {
		fields["ttl"] = opts.TTL
	}
	if opts.Metadata != nil {
		fields["metadata"] = opts.Metadata
	}
	var body any
	if len(fields) > 0 {
		body = fields
	}

	data, err := c.post("/register", body)
	if err != nil {
		return nil, err
	}

	// Multiple hooks response
	if raw, ok := data["hooks"]; ok {
		rawHooks, ok := raw.([]any)
		if !ok {
			return nil, &Error{Message: "invalid hooks response format"}
		}
		hooks := make([]Hook, 0, len(rawHooks))
		for _, rh := range rawHooks {
			h, err := parseHook(rh)
			if err != nil {
				return nil, err
			}
			hooks = append(hooks, h)
		}
		return hooks, nil
	}

	// Single hook response
	h, err := parseHook(data)
	if err != nil {
		return nil, err
	}
	return []Hook{h}, nil
}

// Poll retrieves interactions for a single hook.
func (c *Client) Poll(hookID string) ([]Interaction, error) {
	data, err := c.get("/poll/" + hookID)
	if err != nil {
		return nil, err
	}

	raw, ok := data["interactions"]
	if !ok {
		return []Interaction{}, nil
	}

	return parseInteractions(raw)
}

// PollBatch retrieves interactions for multiple hooks in a single request.
func (c *Client) PollBatch(hookIDs []string) (map[string]BatchResult, error) {
	if len(hookIDs) == 0 {
		return nil, fmt.Errorf("hook_ids must be a non-empty array")
	}

	data, err := c.post("/poll", hookIDs)
	if err != nil {
		return nil, err
	}

	rawResults, ok := data["results"]
	if !ok {
		return nil, &Error{Message: "invalid batch poll response format"}
	}

	resultsMap, ok := rawResults.(map[string]any)
	if !ok {
		return nil, &Error{Message: "invalid batch poll results format"}
	}

	results := make(map[string]BatchResult, len(resultsMap))
	for id, raw := range resultsMap {
		entry, ok := raw.(map[string]any)
		if !ok {
			results[id] = BatchResult{Error: "invalid result format"}
			continue
		}

		br := BatchResult{}
		if errMsg, ok := entry["error"].(string); ok {
			br.Error = errMsg
		}
		if rawInts, ok := entry["interactions"]; ok {
			ints, err := parseInteractions(rawInts)
			if err == nil {
				br.Interactions = ints
			}
		}
		if br.Interactions == nil {
			br.Interactions = []Interaction{}
		}
		results[id] = br
	}

	return results, nil
}

// Metrics retrieves server metrics.
func (c *Client) Metrics() (Metrics, error) {
	return c.get("/metrics")
}

// Activity lists the long-lived hooks that currently have pending interactions,
// so callers can discover which of their long-lived hooks fired without polling
// each one. Drain the details with Poll. Returns an empty slice when none have
// fired (or the server has long-lived hooks disabled).
func (c *Client) Activity() ([]HookActivity, error) {
	data, err := c.get("/activity")
	if err != nil {
		return nil, err
	}

	raw, ok := data["hooks"]
	if !ok {
		return []HookActivity{}, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, &Error{Message: "invalid activity response format"}
	}

	activity := make([]HookActivity, 0, len(arr))
	for _, item := range arr {
		b, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var a HookActivity
		if err := json.Unmarshal(b, &a); err != nil {
			continue
		}
		activity = append(activity, a)
	}
	return activity, nil
}

// HTTP helpers

func (c *Client) get(path string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, c.server+path, nil)
	if err != nil {
		return nil, &ConnectionError{Message: fmt.Sprintf("failed to create request: %v", err)}
	}
	req.Header.Set("X-API-Key", c.token)

	return c.doRequest(req)
}

func (c *Client) post(path string, body any) (map[string]any, error) {
	var bodyReader io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, &Error{Message: fmt.Sprintf("failed to marshal request body: %v", err)}
		}
		bodyReader = bytes.NewReader(jsonBody)
	}

	req, err := http.NewRequest(http.MethodPost, c.server+path, bodyReader)
	if err != nil {
		return nil, &ConnectionError{Message: fmt.Sprintf("failed to create request: %v", err)}
	}
	req.Header.Set("X-API-Key", c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return c.doRequest(req)
}

func (c *Client) doRequest(req *http.Request) (map[string]any, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &ConnectionError{Message: fmt.Sprintf("connection error: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	// Status is decided before the body is touched: none of these errors quote
	// it, so a failing response never has to be buffered at all.
	switch {
	case resp.StatusCode == 401:
		return nil, &AuthenticationError{Message: "authentication failed", StatusCode: 401}
	case resp.StatusCode == 404:
		return nil, &NotFoundError{Message: "resource not found", StatusCode: 404}
	case resp.StatusCode >= 500:
		return nil, &ServerError{Message: fmt.Sprintf("server error: %d", resp.StatusCode), StatusCode: resp.StatusCode}
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, &Error{Message: fmt.Sprintf("unexpected status: %d", resp.StatusCode), StatusCode: resp.StatusCode}
	}

	// Read one byte past the ceiling so an over-limit response is detected
	// without buffering the whole of it.
	body := io.Reader(resp.Body)
	if c.maxResponseBytes > 0 {
		body = io.LimitReader(resp.Body, c.maxResponseBytes+1)
	}

	respBody, err := io.ReadAll(body)
	if err != nil {
		return nil, &ConnectionError{Message: fmt.Sprintf("failed to read response: %v", err)}
	}

	if c.maxResponseBytes > 0 && int64(len(respBody)) > c.maxResponseBytes {
		return nil, &ResponseTooLargeError{
			Message: fmt.Sprintf("response exceeds %d bytes", c.maxResponseBytes),
			Limit:   c.maxResponseBytes,
		}
	}

	if len(respBody) == 0 {
		return nil, &Error{Message: "empty response body"}
	}

	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, &Error{Message: fmt.Sprintf("invalid JSON response: %v", err)}
	}

	return result, nil
}

// Parsing helpers

func parseHook(raw any) (Hook, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return Hook{}, &Error{Message: "invalid hook format"}
	}

	b, err := json.Marshal(m)
	if err != nil {
		return Hook{}, &Error{Message: "failed to parse hook"}
	}

	var h Hook
	if err := json.Unmarshal(b, &h); err != nil {
		return Hook{}, &Error{Message: "failed to parse hook"}
	}
	return h, nil
}

func parseInteractions(raw any) ([]Interaction, error) {
	arr, ok := raw.([]any)
	if !ok {
		return nil, &Error{Message: "invalid interactions format"}
	}

	interactions := make([]Interaction, 0, len(arr))
	for _, item := range arr {
		b, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var i Interaction
		if err := json.Unmarshal(b, &i); err != nil {
			continue
		}
		interactions = append(interactions, i)
	}
	return interactions, nil
}
