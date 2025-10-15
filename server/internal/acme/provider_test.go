package acme

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/libdns/libdns"
)

func TestNewProvider(t *testing.T) {
	logger := slog.Default()
	provider := NewProvider(logger)

	if provider == nil {
		t.Fatal("expected provider to be created")
	}

	if provider.recordMap == nil {
		t.Error("expected recordMap to be initialized")
	}
}

func TestProvider_AppendRecords(t *testing.T) {
	logger := slog.Default()
	provider := NewProvider(logger)
	ctx := context.Background()

	records := []libdns.Record{
		libdns.RR{
			Type: "TXT",
			Name: "_acme-challenge.example.com",
			Data: "test-value-1",
			TTL:  time.Duration(300) * time.Second,
		},
		libdns.RR{
			Type: "TXT",
			Name: "_acme-challenge.example.com",
			Data: "test-value-2",
			TTL:  time.Duration(300) * time.Second,
		},
	}

	added, err := provider.AppendRecords(ctx, "example.com.", records)
	if err != nil {
		t.Fatalf("failed to append records: %v", err)
	}

	if len(added) != 2 {
		t.Errorf("expected 2 records added, got %d", len(added))
	}

	// Verify records were stored
	stored, err := provider.GetRecords(ctx, "example.com.")
	if err != nil {
		t.Fatalf("failed to get records: %v", err)
	}

	if len(stored) != 2 {
		t.Errorf("expected 2 records stored, got %d", len(stored))
	}
}

func TestProvider_GetRecords(t *testing.T) {
	logger := slog.Default()
	provider := NewProvider(logger)
	ctx := context.Background()

	t.Run("empty zone", func(t *testing.T) {
		records, err := provider.GetRecords(ctx, "empty.com.")
		if err != nil {
			t.Fatalf("failed to get records: %v", err)
		}

		if len(records) != 0 {
			t.Errorf("expected 0 records, got %d", len(records))
		}
	})

	t.Run("with records", func(t *testing.T) {
		records := []libdns.Record{
			libdns.RR{
				Type: "TXT",
				Name: "test",
				Data: "value",
				TTL:  time.Duration(300) * time.Second,
			},
		}

		provider.AppendRecords(ctx, "test.com.", records)

		stored, err := provider.GetRecords(ctx, "test.com.")
		if err != nil {
			t.Fatalf("failed to get records: %v", err)
		}

		if len(stored) != 1 {
			t.Errorf("expected 1 record, got %d", len(stored))
		}
	})
}

func TestProvider_DeleteRecords(t *testing.T) {
	logger := slog.Default()
	provider := NewProvider(logger)
	ctx := context.Background()

	records := []libdns.Record{
		libdns.RR{
			Type: "TXT",
			Name: "_acme-challenge",
			Data: "value1",
			TTL:  time.Duration(300) * time.Second,
		},
		libdns.RR{
			Type: "TXT",
			Name: "_acme-challenge",
			Data: "value2",
			TTL:  time.Duration(300) * time.Second,
		},
	}

	// Add records first
	provider.AppendRecords(ctx, "example.com.", records)

	// Delete one record
	toDelete := []libdns.Record{records[0]}
	deleted, err := provider.DeleteRecords(ctx, "example.com.", toDelete)
	if err != nil {
		t.Fatalf("failed to delete records: %v", err)
	}

	if len(deleted) != 1 {
		t.Errorf("expected 1 record deleted, got %d", len(deleted))
	}

	// Verify only one record remains
	remaining, _ := provider.GetRecords(ctx, "example.com.")
	if len(remaining) != 1 {
		t.Errorf("expected 1 record remaining, got %d", len(remaining))
	}
}

func TestProvider_DeleteRecords_EmptyZone(t *testing.T) {
	logger := slog.Default()
	provider := NewProvider(logger)
	ctx := context.Background()

	records := []libdns.Record{
		libdns.RR{
			Type: "TXT",
			Name: "test",
			Data: "value",
			TTL:  time.Duration(300) * time.Second,
		},
	}

	deleted, err := provider.DeleteRecords(ctx, "nonexistent.com.", records)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if deleted != nil {
		t.Errorf("expected nil deleted records, got %v", deleted)
	}
}

func TestProvider_SetRecords(t *testing.T) {
	logger := slog.Default()
	provider := NewProvider(logger)
	ctx := context.Background()

	// Add initial records
	initial := []libdns.Record{
		libdns.RR{
			Type: "TXT",
			Name: "old",
			Data: "value",
			TTL:  time.Duration(300) * time.Second,
		},
	}
	provider.AppendRecords(ctx, "example.com.", initial)

	// Set new records (should replace)
	newRecords := []libdns.Record{
		libdns.RR{
			Type: "TXT",
			Name: "new",
			Data: "value",
			TTL:  time.Duration(300) * time.Second,
		},
	}

	set, err := provider.SetRecords(ctx, "example.com.", newRecords)
	if err != nil {
		t.Fatalf("failed to set records: %v", err)
	}

	if len(set) != 1 {
		t.Errorf("expected 1 record set, got %d", len(set))
	}

	// Verify old records are replaced
	stored, _ := provider.GetRecords(ctx, "example.com.")
	if len(stored) != 1 {
		t.Errorf("expected 1 record, got %d", len(stored))
	}

	if stored[0].RR().Name != "new" {
		t.Errorf("expected name 'new', got %s", stored[0].RR().Name)
	}
}

func TestCompareRecords(t *testing.T) {
	rec1 := libdns.RR{
		Type: "TXT",
		Name: "test",
		Data: "value",
		TTL:  time.Duration(300) * time.Second,
	}

	rec2 := libdns.RR{
		Type: "TXT",
		Name: "test",
		Data: "value",
		TTL:  time.Duration(300) * time.Second,
	}

	rec3 := libdns.RR{
		Type: "TXT",
		Name: "different",
		Data: "value",
		TTL:  time.Duration(300) * time.Second,
	}

	if !compareRecords(rec1, rec2) {
		t.Error("expected identical records to be equal")
	}

	if compareRecords(rec1, rec3) {
		t.Error("expected different records to not be equal")
	}
}

func TestRecordStore_DeleteRecords(t *testing.T) {
	rec1 := libdns.RR{
		Type: "TXT",
		Name: "rec1",
		Data: "value1",
		TTL:  time.Duration(300) * time.Second,
	}
	rec2 := libdns.RR{
		Type: "TXT",
		Name: "rec2",
		Data: "value2",
		TTL:  time.Duration(300) * time.Second,
	}

	store := &RecordStore{
		entries: []libdns.Record{rec1, rec2},
	}

	toDelete := []libdns.Record{store.entries[0]}
	deleted := store.deleteRecords(toDelete)

	if len(deleted) != 1 {
		t.Errorf("expected 1 deleted record, got %d", len(deleted))
	}

	if len(store.entries) != 1 {
		t.Errorf("expected 1 remaining record, got %d", len(store.entries))
	}

	if store.entries[0].RR().Name != "rec2" {
		t.Errorf("expected remaining record 'rec2', got %s", store.entries[0].RR().Name)
	}
}
