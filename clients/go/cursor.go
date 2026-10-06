package hookd

import (
	"fmt"
	"net/url"
)

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
	if err := validateCursors(field, cursors); err != nil {
		return nil, err
	}

	data, err := c.post(path, map[string]any{field: cursors})
	if err != nil {
		return nil, err
	}
	return parseCursorEntries(data)
}

func validateCursors(field string, cursors map[string]int64) error {
	if len(cursors) == 0 {
		return fmt.Errorf("%s must not be empty", field)
	}
	for _, seq := range cursors {
		if seq < 0 {
			return fmt.Errorf("%s values must not be negative", field)
		}
	}

	return nil
}

// parseCursorEntries retains per-hook errors without rejecting valid siblings.
func parseCursorEntries(data map[string]any) (map[string]map[string]any, error) {
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
