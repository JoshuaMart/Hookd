package hookd

import (
	"encoding/json"
	"net/url"
)

// Metrics retrieves server metrics.
func (c *Client) Metrics() (Metrics, error) {
	return c.get("/metrics")
}

// Activity lists the long-lived hooks that currently have pending interactions,
// so callers can discover which of their long-lived hooks fired without polling
// each one. Fetch the details with Read (or Poll to drain). Returns an empty slice when none have
// fired (or the server has long-lived hooks disabled).
func (c *Client) Activity() ([]HookActivity, error) {
	return c.ActivityMatching(nil)
}

// ActivityMatching is Activity restricted to hooks whose metadata matches every
// key/value in the filter.
func (c *Client) ActivityMatching(metadata map[string]string) ([]HookActivity, error) {
	data, err := c.get("/activity" + metadataQuery(metadata))
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

// metadataQuery encodes a metadata filter as metadata.<key>=<value> parameters.
func metadataQuery(metadata map[string]string) string {
	if len(metadata) == 0 {
		return ""
	}
	q := url.Values{}
	for k, v := range metadata {
		q.Set("metadata."+k, v)
	}
	return "?" + q.Encode()
}
