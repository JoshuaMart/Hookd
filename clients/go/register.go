package hookd

import (
	"fmt"
)

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
	if _, ok := data["hooks"]; ok {
		return parseHookList(data)
	}

	// Single hook response
	h, err := parseHook(data)
	if err != nil {
		return nil, err
	}
	return []Hook{h}, nil
}

// RegisterBatch creates one hook per spec, each with its own TTL and metadata,
// in one request. Hooks come back in spec order; the server creates all or none.
func (c *Client) RegisterBatch(specs []HookSpec) ([]Hook, error) {
	if len(specs) == 0 {
		return nil, fmt.Errorf("specs must not be empty")
	}
	data, err := c.post("/register", map[string]any{"hooks": specs})
	if err != nil {
		return nil, err
	}
	return parseHookList(data)
}

// Hooks lists the long-lived hooks, without interactions, whose metadata
// matches every key/value in the filter (nil lists all).
func (c *Client) Hooks(metadata map[string]string) ([]Hook, error) {
	data, err := c.get("/hooks" + metadataQuery(metadata))
	if err != nil {
		return nil, err
	}
	return parseHookList(data)
}
