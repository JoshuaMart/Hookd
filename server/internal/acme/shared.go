package acme

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/libdns/libdns"
	"github.com/redis/go-redis/v9"
)

// SharedRecords keeps challenge records where every instance can read them.
// Records compare on type, name, data and TTL, like compareRecords.
type SharedRecords interface {
	Append(ctx context.Context, zone string, recs []libdns.Record) error
	Delete(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error)
	Get(ctx context.Context, zone string) ([]libdns.Record, error)
	Set(ctx context.Context, zone string, recs []libdns.Record) error
}

func (p *Provider) sharedAppend(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	p.logger.Info("acme: appending shared records", "zone", normalizeZone(zone), "count", len(recs))
	if err := p.shared.Append(ctx, zone, recs); err != nil {
		return nil, err
	}
	return recs, nil
}

func (p *Provider) sharedDelete(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	p.logger.Info("acme: deleting shared records", "zone", normalizeZone(zone), "count", len(recs))
	return p.shared.Delete(ctx, zone, recs)
}

func (p *Provider) sharedSet(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	p.logger.Info("acme: setting shared records", "zone", normalizeZone(zone), "count", len(recs))
	if err := p.shared.Set(ctx, zone, recs); err != nil {
		return nil, err
	}
	return recs, nil
}

func normalizeZone(zone string) string {
	return strings.ToLower(strings.TrimSuffix(zone, "."))
}

// recordTTL bounds how long a challenge record outlives a crashed instance
// that never deleted it. A DNS-01 challenge completes within minutes.
const recordTTL = time.Hour

// RedisRecords keeps each zone's records in a Redis set of their JSON form,
// which doubles as their identity.
type RedisRecords struct {
	client redis.UniversalClient
	prefix string
}

// NewRedisRecords stores records under prefix + "acme:<zone>".
func NewRedisRecords(client redis.UniversalClient, prefix string) *RedisRecords {
	return &RedisRecords{client: client, prefix: prefix}
}

func (r *RedisRecords) key(zone string) string { return r.prefix + "acme:" + zone }

func encodeRecords(recs []libdns.Record) ([]any, error) {
	members := make([]any, len(recs))
	for i, rec := range recs {
		b, err := json.Marshal(rec.RR())
		if err != nil {
			return nil, fmt.Errorf("encode acme record: %w", err)
		}
		members[i] = string(b)
	}
	return members, nil
}

// Append adds records and restarts the zone's expiry.
func (r *RedisRecords) Append(ctx context.Context, zone string, recs []libdns.Record) error {
	if len(recs) == 0 {
		return nil
	}
	members, err := encodeRecords(recs)
	if err != nil {
		return err
	}
	_, err = r.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.SAdd(ctx, r.key(zone), members...)
		pipe.Expire(ctx, r.key(zone), recordTTL)
		return nil
	})
	return err
}

// Delete removes records and returns the ones that were present.
func (r *RedisRecords) Delete(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	members, err := encodeRecords(recs)
	if err != nil {
		return nil, err
	}
	cmds := make([]*redis.IntCmd, len(members))
	if _, err := r.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		for i, m := range members {
			cmds[i] = pipe.SRem(ctx, r.key(zone), m)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	var deleted []libdns.Record
	for i, cmd := range cmds {
		if cmd.Val() == 1 {
			deleted = append(deleted, recs[i])
		}
	}
	return deleted, nil
}

// Get returns the zone's records.
func (r *RedisRecords) Get(ctx context.Context, zone string) ([]libdns.Record, error) {
	members, err := r.client.SMembers(ctx, r.key(zone)).Result()
	if err != nil {
		return nil, err
	}
	recs := make([]libdns.Record, 0, len(members))
	for _, m := range members {
		var rr libdns.RR
		if err := json.Unmarshal([]byte(m), &rr); err != nil {
			return nil, fmt.Errorf("decode acme record: %w", err)
		}
		recs = append(recs, rr)
	}
	return recs, nil
}

// Set replaces the zone's records.
func (r *RedisRecords) Set(ctx context.Context, zone string, recs []libdns.Record) error {
	members, err := encodeRecords(recs)
	if err != nil {
		return err
	}
	_, err = r.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Del(ctx, r.key(zone))
		if len(members) > 0 {
			pipe.SAdd(ctx, r.key(zone), members...)
			pipe.Expire(ctx, r.key(zone), recordTTL)
		}
		return nil
	})
	return err
}
