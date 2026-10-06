package hookd

import (
	"encoding/json"
)

// parseHookList reads a {"hooks": [...]} response.
func parseHookList(data map[string]any) ([]Hook, error) {
	raw, ok := data["hooks"].([]any)
	if !ok {
		return nil, &Error{Message: "invalid hooks response format"}
	}
	hooks := make([]Hook, 0, len(raw))
	for _, rh := range raw {
		h, err := parseHook(rh)
		if err != nil {
			return nil, err
		}
		hooks = append(hooks, h)
	}
	return hooks, nil
}

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
