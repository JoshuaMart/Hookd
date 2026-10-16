package acme

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/libdns/libdns"
	"github.com/redis/go-redis/v9"
)

// newSharedProviders returns two providers on one fresh prefix of
// HOOKD_TEST_REDIS_ADDR: two instances behind a load balancer.
func newSharedProviders(t *testing.T) (*Provider, *Provider, *redis.Client, string) {
	t.Helper()
	addr := os.Getenv("HOOKD_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("HOOKD_TEST_REDIS_ADDR is not set")
	}
	b := make([]byte, 6)
	rand.Read(b)
	prefix := "hookdtest:" + hex.EncodeToString(b) + ":"
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() {
		ctx := context.Background()
		if keys, _ := client.Keys(ctx, prefix+"*").Result(); len(keys) > 0 {
			client.Del(ctx, keys...)
		}
		client.Close()
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	records := NewRedisRecords(client, prefix)
	return NewSharedProvider(logger, records), NewSharedProvider(logger, records), client, prefix
}

func txt(data string) libdns.RR {
	return libdns.RR{Type: "TXT", Name: "_acme-challenge", Data: data, TTL: 2 * time.Minute}
}

func TestSharedProvider_RecordsReachEveryInstance(t *testing.T) {
	a, b, client, prefix := newSharedProviders(t)
	ctx := context.Background()

	if _, err := a.AppendRecords(ctx, "example.com.", []libdns.Record{txt("one"), txt("two")}); err != nil {
		t.Fatal(err)
	}
	got, err := b.GetRecords(ctx, "example.com.")
	if err != nil || len(got) != 2 {
		t.Fatalf("the other instance must see both records, got %v, %v", got, err)
	}
	for _, rec := range got {
		if rr := rec.RR(); rr.Type != "TXT" || rr.Name != "_acme-challenge" || rr.TTL != 2*time.Minute {
			t.Errorf("record changed in transit: %+v", rr)
		}
	}
	if ttl, _ := client.TTL(ctx, prefix+"acme:example.com.").Result(); ttl <= 0 || ttl > recordTTL {
		t.Errorf("expected the zone to expire within %s, got %s", recordTTL, ttl)
	}

	deleted, err := b.DeleteRecords(ctx, "example.com.", []libdns.Record{txt("one"), txt("absent")})
	if err != nil || len(deleted) != 1 || deleted[0].RR().Data != "one" {
		t.Fatalf("expected only the present record deleted, got %v, %v", deleted, err)
	}
	if got, _ := a.GetRecords(ctx, "example.com."); len(got) != 1 || got[0].RR().Data != "two" {
		t.Errorf("expected the remaining record, got %v", got)
	}
}

func TestSharedProvider_SetReplacesAndZonesAreSeparate(t *testing.T) {
	a, b, _, _ := newSharedProviders(t)
	ctx := context.Background()

	a.AppendRecords(ctx, "example.com.", []libdns.Record{txt("old")})
	a.AppendRecords(ctx, "other.test.", []libdns.Record{txt("kept")})
	if _, err := b.SetRecords(ctx, "example.com.", []libdns.Record{txt("new")}); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.GetRecords(ctx, "example.com."); len(got) != 1 || got[0].RR().Data != "new" {
		t.Errorf("expected Set to replace the zone, got %v", got)
	}
	if got, _ := a.GetRecords(ctx, "other.test."); len(got) != 1 {
		t.Errorf("another zone must be untouched, got %v", got)
	}

	if _, err := b.SetRecords(ctx, "example.com.", nil); err != nil {
		t.Fatal(err)
	}
	if got, err := a.GetRecords(ctx, "example.com."); err != nil || len(got) != 0 {
		t.Errorf("expected an empty zone, got %v, %v", got, err)
	}
}

func TestSharedProvider_StorageErrorsSurface(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	defer client.Close()
	p := NewSharedProvider(slog.New(slog.NewTextHandler(io.Discard, nil)), NewRedisRecords(client, "x:"))
	ctx := context.Background()

	// CertMagic must see the failure, or it would wait for a record no
	// instance can serve.
	if _, err := p.AppendRecords(ctx, "example.com.", []libdns.Record{txt("v")}); err == nil {
		t.Error("expected an append error")
	}
	if _, err := p.GetRecords(ctx, "example.com."); err == nil {
		t.Error("expected a get error")
	}
}
