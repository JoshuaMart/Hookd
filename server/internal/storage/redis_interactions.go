package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// pollChunkSize bounds the hooks drained by one script: Redis runs nothing
// else while a script runs.
const pollChunkSize = 64

// AddInteraction stores an interaction and sets its Seq. Interactions for
// unknown hooks are dropped.
func (m *RedisManager) AddInteraction(hookID string, interaction *Interaction) {
	args, err := m.addArgs(hookID, interaction)
	if err != nil {
		m.logFailure("encode interaction", err, "hook_id", hookID)
		return
	}
	seq, err := addInteractionScript.Run(context.Background(), m.client, nil, args...).Int64()
	if err != nil {
		m.logFailure("add interaction", err, "hook_id", hookID)
		return
	}
	if seq > 0 {
		interaction.Seq = seq
	}
}

// addArgs builds add_interaction.lua's arguments, prefix included.
func (m *RedisManager) addArgs(hookID string, it *Interaction) ([]any, error) {
	full, longLived, err := m.encodeInteraction(it)
	if err != nil {
		return nil, err
	}
	return []any{m.prefix, hookID, m.maxPerHook, micros(it.Timestamp), string(it.Type), full, longLived}, nil
}

// encodeInteraction returns the stored JSON and, when the body exceeds the
// long-lived cap, the truncated form the script stores for a long-lived hook.
// Which one applies is only known inside the script.
func (m *RedisManager) encodeInteraction(it *Interaction) (full, longLived string, err error) {
	b, err := json.Marshal(it)
	if err != nil {
		return "", "", err
	}
	body, ok := it.Data["body"].(string)
	if !ok {
		return string(b), "", nil
	}
	cut, truncated := TruncateBody(body, m.maxBodyBytes)
	if !truncated {
		return string(b), "", nil
	}
	short := *it
	short.Data = maps.Clone(it.Data)
	short.Data["body"] = cut
	short.Data["truncated"] = true
	ll, err := json.Marshal(&short)
	if err != nil {
		return "", "", err
	}
	return string(b), string(ll), nil
}

// ReadInteractions returns the interactions past the cursor without deleting them.
func (m *RedisManager) ReadInteractions(hookID string, after int64) (CursorRead, error) {
	reply, err := m.run(context.Background(), readScript, hookID, startAfter(after)).Slice()
	if errors.Is(err, redis.Nil) {
		return CursorRead{}, ErrHookNotFound
	}
	if err != nil {
		return CursorRead{}, err
	}
	return decodeRead(reply)
}

// decodeRead parses read.lua's {dropped_through, entries} reply.
func decodeRead(reply []any) (CursorRead, error) {
	if len(reply) != 2 {
		return CursorRead{}, fmt.Errorf("unexpected read reply of length %d", len(reply))
	}
	dropped, _ := reply[0].(string)
	through, err := strconv.ParseInt(dropped, 10, 64)
	if err != nil {
		return CursorRead{}, fmt.Errorf("parse dropped_through %q: %w", dropped, err)
	}
	interactions, err := decodeEntries(reply[1])
	if err != nil {
		return CursorRead{}, err
	}
	return CursorRead{Interactions: interactions, DroppedThrough: through}, nil
}

// maxSeq bounds client-supplied cursors so seq+1 cannot overflow.
const maxSeq = 1 << 62

func clampSeq(seq int64) int64 { return min(max(seq, 0), maxSeq) }

// startAfter is the first stream ID past a cursor.
func startAfter(after int64) string { return streamID(clampSeq(after) + 1) }

// AckInteractions deletes the interactions up to and including through.
func (m *RedisManager) AckInteractions(hookID string, through int64) (int, error) {
	through = clampSeq(through)
	n, err := m.run(context.Background(), ackScript, hookID, streamID(through), streamID(through+1)).Int64()
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, ErrHookNotFound
	}
	return int(n), nil
}

// PollInteractions drains a hook's interactions.
func (m *RedisManager) PollInteractions(hookID string) []*Interaction {
	result := m.PollInteractionsBatch([]string{hookID})[hookID]
	if result == nil || result.Error != "" {
		return []*Interaction{}
	}
	return result.Interactions
}

// PollInteractionsBatch drains several hooks. Unknown hooks yield a not-found
// error, matching the other backends.
func (m *RedisManager) PollInteractionsBatch(hookIDs []string) map[string]*PollResult {
	ids := uniqueIDs(hookIDs)
	results := make(map[string]*PollResult, len(ids))
	for start := 0; start < len(ids); start += pollChunkSize {
		m.pollChunk(ids[start:min(start+pollChunkSize, len(ids))], results)
	}
	return results
}

// uniqueIDs drops repeats, so a duplicate cannot replace the first drain
// with an empty one.
func uniqueIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// pollChunk drains one script's worth of hooks into results.
func (m *RedisManager) pollChunk(ids []string, results map[string]*PollResult) {
	reply, err := m.run(context.Background(), pollScript, idArgs(ids)...).Slice()
	if err == nil && len(reply) != len(ids) {
		err = fmt.Errorf("unexpected poll reply of length %d", len(reply))
	}
	if err != nil {
		m.logFailure("poll", err)
		for _, id := range ids {
			results[id] = &PollResult{Error: "storage error"}
		}
		return
	}
	for i, id := range ids {
		results[id] = m.pollResult(id, reply[i])
	}
}

// pollResult decodes one hook's share of a poll reply; nil means unknown.
func (m *RedisManager) pollResult(id string, reply any) *PollResult {
	if reply == nil {
		return &PollResult{Error: "Hook not found"}
	}
	interactions, err := decodeEntries(reply)
	if err != nil {
		// The script already deleted them: report the loss.
		m.logFailure("decode polled interactions", err, "hook_id", id)
		return &PollResult{Error: "storage error"}
	}
	return &PollResult{Interactions: interactions}
}

// idArgs appends hook IDs to leading script arguments.
func idArgs(ids []string, lead ...any) []any {
	args := make([]any, 0, len(lead)+len(ids))
	args = append(args, lead...)
	for _, id := range ids {
		args = append(args, id)
	}
	return args
}

// decodeEntries decodes stream entries as a script returns them.
func decodeEntries(v any) ([]*Interaction, error) {
	entries, ok := v.([]any)
	if !ok && v != nil {
		return nil, fmt.Errorf("unexpected entries reply %T", v)
	}
	out := make([]*Interaction, 0, len(entries))
	for _, e := range entries {
		entry, ok := e.([]any)
		if !ok || len(entry) != 2 {
			return nil, fmt.Errorf("unexpected stream entry %v", e)
		}
		id, _ := entry[0].(string)
		fields, _ := entry[1].([]any)
		var data string
		for i := 0; i+1 < len(fields); i += 2 {
			if fields[i] == "j" {
				data, _ = fields[i+1].(string)
			}
		}
		it, err := decodeInteraction(id, data)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, nil
}

// decodeInteraction takes the seq from the stream ID, which is authoritative.
func decodeInteraction(streamID, data string) (*Interaction, error) {
	seq, err := strconv.ParseInt(strings.SplitN(streamID, "-", 2)[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse stream id %q: %w", streamID, err)
	}
	var it Interaction
	if err := json.Unmarshal([]byte(data), &it); err != nil {
		return nil, fmt.Errorf("decode interaction %s: %w", streamID, err)
	}
	it.Seq = seq
	return &it, nil
}
