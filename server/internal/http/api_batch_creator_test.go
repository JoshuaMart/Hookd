package http

import (
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/eviction"
	"github.com/jomar/hookd/internal/storage"
)

// batchStore stands in for a shared store whose hook creation can fail.
type batchStore struct {
	*storage.MemoryManager
	err     error
	batches [][]storage.CreateOptions
	max     int
}

func (s *batchStore) CreateHookBatch(domain string, opts []storage.CreateOptions, maxLongLived int) ([]*storage.Hook, error) {
	s.batches = append(s.batches, opts)
	s.max = maxLongLived
	if s.err != nil {
		return nil, s.err
	}
	hooks := make([]*storage.Hook, len(opts))
	for i, o := range opts {
		hooks[i] = s.CreateHook(domain, o)
	}
	return hooks, nil
}

func newBatchHandler(t *testing.T, store *batchStore) *APIHandler {
	t.Helper()
	evictor := eviction.NewEvictor(store, config.EvictionConfig{
		HookTTL: testEphemeralTTL, InteractionTTL: time.Hour, MaxPerHook: 100, MaxMemoryMB: 1000, CleanupInterval: time.Second,
	}, slog.Default())
	return NewAPIHandler(APIHandlerOptions{
		Storage: store,
		Evictor: evictor,
		Domain:  "example.com",
		LongLived: config.LongLivedConfig{
			Enabled: true, MaxTTL: 720 * time.Hour, MaxHooks: 3, MaxInteractionBodyBytes: 1024, MaxMetadataBytes: 64,
		},
		Logger: slog.Default(),
	})
}

func TestRegister_BatchCreatorGetsTheWholeBatch(t *testing.T) {
	store := &batchStore{MemoryManager: storage.NewMemoryManager(func() string { return "id" })}
	w := registerBody(t, newBatchHandler(t, store), `{"hooks":[{},{"ttl":"48h"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if len(store.batches) != 1 || len(store.batches[0]) != 2 || store.max != 3 {
		t.Fatalf("expected one batch of both hooks under the long-lived cap, got %v (max %d)", store.batches, store.max)
	}
}

func TestRegister_BatchCreatorFailures(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{
		{storage.ErrHookLimitReached, http.StatusTooManyRequests},
		// An ephemeral hook that was never stored must not be handed out.
		{errors.New("redis: connection refused"), http.StatusInternalServerError},
	} {
		store := &batchStore{MemoryManager: storage.NewMemoryManager(func() string { return "id" }), err: tc.err}
		if w := registerBody(t, newBatchHandler(t, store), ``); w.Code != tc.code {
			t.Errorf("%v: expected %d, got %d (%s)", tc.err, tc.code, w.Code, w.Body.String())
		}
	}
}
