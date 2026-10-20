package storage

import (
	"context"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// Stats reports the shared counts, which cover every instance, and this
// process's own memory statistics.
func (m *RedisManager) Stats() Stats {
	stats := Stats{Memory: runtimeMemoryStats()}
	vals, err := m.client.HMGet(context.Background(), m.key("stats"), "hooks", "dns", "http", "smtp").Result()
	if err != nil {
		m.logFailure("read stats", err)
		return stats
	}
	count := func(v any) int {
		s, _ := v.(string)
		n, _ := strconv.Atoi(s)
		return n
	}
	stats.HooksActive = count(vals[0])
	stats.InteractionsDNS = count(vals[1])
	stats.InteractionsHTTP = count(vals[2])
	stats.InteractionsSMTP = count(vals[3])
	stats.InteractionsTotal = stats.InteractionsDNS + stats.InteractionsHTTP + stats.InteractionsSMTP
	return stats
}

// LongLivedActivity returns the long-lived hooks of owner with pending
// interactions, most recent activity first.
func (m *RedisManager) LongLivedActivity(owner string) []HookActivity {
	ctx := context.Background()
	ids, err := m.activeIDs(ctx, owner)
	if err != nil {
		m.logFailure("list long-lived activity", err)
		return nil
	}
	if len(ids) == 0 {
		return nil
	}

	type activityCmds struct {
		hook    *redis.MapStringStringCmd
		pending *redis.IntCmd
		last    *redis.XMessageSliceCmd
	}
	pipe := m.client.Pipeline()
	cmds := make([]activityCmds, len(ids))
	for i, id := range ids {
		cmds[i] = activityCmds{
			hook:    pipe.HGetAll(ctx, m.hookKey(id)),
			pending: pipe.XLen(ctx, m.streamKey(id)),
			last:    pipe.XRevRangeN(ctx, m.streamKey(id), "+", "-", 1),
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		m.logFailure("read long-lived activity", err)
		return nil
	}

	activity := make([]HookActivity, 0, len(ids))
	for i, c := range cmds {
		fields, last := c.hook.Val(), c.last.Val()
		if len(fields) == 0 || len(last) == 0 {
			continue // acknowledged or expired since the index was read
		}
		if !ownedBy(fields["owner"], owner) {
			continue
		}
		data, _ := last[0].Values["j"].(string)
		it, err := decodeInteraction(last[0].ID, data)
		if err != nil {
			m.logFailure("decode long-lived activity", err, "hook_id", ids[i])
			continue
		}
		activity = append(activity, HookActivity{
			Hook:              decodeHook(ids[i], fields),
			PendingCount:      int(c.pending.Val()),
			LastInteractionAt: it.Timestamp,
			LastSeq:           it.Seq,
		})
	}
	return activity
}

// activeIDs lists the active long-lived hooks of owner, latest first. The
// owner's index is intersected with ll:active, keeping the latter's scores.
func (m *RedisManager) activeIDs(ctx context.Context, owner string) ([]string, error) {
	if owner == AnyOwner {
		return m.client.ZRangeArgs(ctx, redis.ZRangeArgs{
			Key: m.key("ll:active"), Start: "+inf", Stop: "-inf", ByScore: true, Rev: true,
		}).Result()
	}
	active, err := m.client.ZInterWithScores(ctx, &redis.ZStore{
		Keys: []string{m.key("ll:active"), m.ownerKey(owner)}, Weights: []float64{1, 0},
	}).Result()
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(active))
	for i, z := range active { // ascending by score
		ids[len(active)-1-i], _ = z.Member.(string)
	}
	return ids, nil
}
