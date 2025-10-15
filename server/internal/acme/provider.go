package acme

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/libdns/libdns"
)

// RecordStore stores DNS records for a zone
type RecordStore struct {
	entries []libdns.Record
}

// Provider implements libdns interfaces for CertMagic DNS-01 challenges
type Provider struct {
	sync.Mutex
	recordMap map[string]*RecordStore
	logger    *slog.Logger
}

// NewProvider creates a new DNS provider for ACME challenges
func NewProvider(logger *slog.Logger) *Provider {
	return &Provider{
		Mutex:     sync.Mutex{},
		recordMap: make(map[string]*RecordStore),
		logger:    logger,
	}
}

func (p *Provider) getZoneRecords(_ context.Context, zoneName string) *RecordStore {
	return p.recordMap[zoneName]
}

func compareRecords(a, b libdns.Record) bool {
	aRR := a.RR()
	bRR := b.RR()
	return aRR.Type == bRR.Type && aRR.Name == bRR.Name && aRR.Data == bRR.Data && aRR.TTL == bRR.TTL
}

func (r *RecordStore) deleteRecords(recs []libdns.Record) []libdns.Record {
	var deletedRecords []libdns.Record
	for i, entry := range r.entries {
		for _, record := range recs {
			if compareRecords(entry, record) {
				deletedRecords = append(deletedRecords, record)
				r.entries = append(r.entries[:i], r.entries[i+1:]...)
			}
		}
	}
	return deletedRecords
}

// AppendRecords adds records to the zone
func (p *Provider) AppendRecords(ctx context.Context, zoneName string, recs []libdns.Record) ([]libdns.Record, error) {
	p.Lock()
	defer p.Unlock()

	zoneRecordStore := p.getZoneRecords(ctx, zoneName)
	if zoneRecordStore == nil {
		zoneRecordStore = new(RecordStore)
		p.recordMap[zoneName] = zoneRecordStore
	}

	// Normalize zone name for logging
	normalizedZone := strings.ToLower(strings.TrimSuffix(zoneName, "."))

	p.logger.Info("acme: appending records", "zone", normalizedZone, "count", len(recs))

	for _, rec := range recs {
		rr := rec.RR()
		p.logger.Info("acme: adding TXT record",
			"name", rr.Name,
			"value", rr.Data,
			"ttl", rr.TTL)
		zoneRecordStore.entries = append(zoneRecordStore.entries, rec)
	}

	return recs, nil
}

// DeleteRecords removes records from the zone
func (p *Provider) DeleteRecords(ctx context.Context, zoneName string, recs []libdns.Record) ([]libdns.Record, error) {
	p.Lock()
	defer p.Unlock()

	zoneRecordStore := p.getZoneRecords(ctx, zoneName)
	if zoneRecordStore == nil {
		return nil, nil
	}

	normalizedZone := strings.ToLower(strings.TrimSuffix(zoneName, "."))
	p.logger.Info("acme: deleting records", "zone", normalizedZone, "count", len(recs))

	deletedRecords := zoneRecordStore.deleteRecords(recs)
	return deletedRecords, nil
}

// GetRecords returns all records from the zone
func (p *Provider) GetRecords(ctx context.Context, zoneName string) ([]libdns.Record, error) {
	p.Lock()
	defer p.Unlock()

	zoneRecordStore := p.getZoneRecords(ctx, zoneName)
	if zoneRecordStore == nil {
		return []libdns.Record{}, nil
	}

	normalizedZone := strings.ToLower(strings.TrimSuffix(zoneName, "."))
	p.logger.Debug("acme: getting records", "zone", normalizedZone, "count", len(zoneRecordStore.entries))

	return zoneRecordStore.entries, nil
}

// SetRecords sets records in the zone (replaces all existing records)
func (p *Provider) SetRecords(ctx context.Context, zoneName string, recs []libdns.Record) ([]libdns.Record, error) {
	p.Lock()
	defer p.Unlock()

	normalizedZone := strings.ToLower(strings.TrimSuffix(zoneName, "."))
	p.logger.Info("acme: setting records", "zone", normalizedZone, "count", len(recs))

	zoneRecordStore := &RecordStore{entries: recs}
	p.recordMap[zoneName] = zoneRecordStore

	return recs, nil
}
