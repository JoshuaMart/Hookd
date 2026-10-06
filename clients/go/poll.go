package hookd

import (
	"fmt"
)

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
