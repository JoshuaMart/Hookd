// Package hookd provides a Go client for the Hookd interaction logging server.
package hookd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const Version = "1.3.0"

// DefaultMaxResponseBytes caps the buffered response body. It is generous
// because a poll can legitimately return many interactions with full bodies.
const DefaultMaxResponseBytes int64 = 64 << 20 // 64 MiB

// Client communicates with a Hookd server.
type Client struct {
	server           string
	token            string
	httpClient       *http.Client
	maxResponseBytes int64
}

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

// ResponseTooLargeError indicates a response above the client's size limit.
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

// SetMaxResponseBytes overrides the response cap. Zero or less disables it.
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

// Read returns the interactions with a seq above after without deleting them.
// Acknowledge them with Ack once they are stored.
func (c *Client) Read(hookID string, after int64) (*CursorRead, error) {
	if after < 0 {
		return nil, fmt.Errorf("after must not be negative")
	}
	data, err := c.get(fmt.Sprintf("/poll/%s?after=%d", url.PathEscape(hookID), after))
	if err != nil {
		return nil, err
	}

	read := &CursorRead{Interactions: []Interaction{}, DroppedThrough: toInt64(data["dropped_through"])}
	if raw, ok := data["interactions"]; ok {
		if read.Interactions, err = parseInteractions(raw); err != nil {
			return nil, err
		}
	}
	if meta, ok := data["metadata"].(map[string]any); ok {
		read.Metadata = meta
	}
	return read, nil
}

// Ack deletes the interactions with a seq up to through and returns how many
// were removed.
func (c *Client) Ack(hookID string, through int64) (int, error) {
	if through < 0 {
		return 0, fmt.Errorf("through must not be negative")
	}
	data, err := c.delete(fmt.Sprintf("/poll/%s?through=%d", url.PathEscape(hookID), through))
	if err != nil {
		return 0, err
	}
	return int(toInt64(data["acknowledged"])), nil
}

// ReadBatch reads several hooks past their cursors (hook ID to seq) in one
// request, without deleting anything.
func (c *Client) ReadBatch(after map[string]int64) (map[string]BatchResult, error) {
	entries, err := c.cursorBatch("/read", "after", after)
	if err != nil {
		return nil, err
	}

	results := make(map[string]BatchResult, len(entries))
	for id, entry := range entries {
		br := BatchResult{Interactions: []Interaction{}, DroppedThrough: toInt64(entry["dropped_through"])}
		br.Error, _ = entry["error"].(string)
		if raw, ok := entry["interactions"]; ok {
			if ints, err := parseInteractions(raw); err == nil {
				br.Interactions = ints
			}
		}
		results[id] = br
	}
	return results, nil
}

// AckBatch acknowledges several hooks (hook ID to seq) in one request.
func (c *Client) AckBatch(through map[string]int64) (map[string]AckResult, error) {
	entries, err := c.cursorBatch("/ack", "through", through)
	if err != nil {
		return nil, err
	}

	results := make(map[string]AckResult, len(entries))
	for id, entry := range entries {
		ar := AckResult{Acknowledged: int(toInt64(entry["acknowledged"]))}
		ar.Error, _ = entry["error"].(string)
		results[id] = ar
	}
	return results, nil
}

// cursorBatch posts {field: cursors} and returns the per-hook result objects.
func (c *Client) cursorBatch(path, field string, cursors map[string]int64) (map[string]map[string]any, error) {
	if len(cursors) == 0 {
		return nil, fmt.Errorf("%s must not be empty", field)
	}
	for _, seq := range cursors {
		if seq < 0 {
			return nil, fmt.Errorf("%s values must not be negative", field)
		}
	}

	data, err := c.post(path, map[string]any{field: cursors})
	if err != nil {
		return nil, err
	}
	raw, ok := data["results"].(map[string]any)
	if !ok {
		return nil, &Error{Message: "invalid batch results format"}
	}

	entries := make(map[string]map[string]any, len(raw))
	for id, r := range raw {
		entry, ok := r.(map[string]any)
		if !ok {
			entry = map[string]any{"error": "invalid result format"}
		}
		entries[id] = entry
	}
	return entries, nil
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
// each one. Fetch the details with Read (or Poll to drain). Returns an empty slice when none have
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

func (c *Client) delete(path string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodDelete, c.server+path, nil)
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

	// Status first: none of these errors quote the body, so it stays unread.
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

	// One byte past the ceiling is enough to detect an over-limit response.
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

// toInt64 reads a JSON number decoded as float64; seqs stay far below 2^53.
func toInt64(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
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
