package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// The contract tests pin the behaviour every Manager backend must share, so a
// new backend is added by appending a factory below, not by porting tests.
// Backend-specific behaviour (SQLite truncation, memory pressure, migrations)
// stays in that backend's own tests.

// fullManager is a backend that also supports long-lived hooks.
type fullManager interface {
	Manager
	LongLivedManager
}

type contractStore[M any] struct {
	name string
	open func(t *testing.T) M
}

// managerStores lists every backend. The two composite variants send hooks
// created with a TTL to memory and to SQLite respectively; hooks without a
// TTL always land in memory.
var managerStores = []contractStore[Manager]{
	{"memory", func(t *testing.T) Manager { return NewMemoryManager(perfIDs()) }},
	{"sqlite", func(t *testing.T) Manager { return newTestSQLiteWithID(t, 1024, perfIDs()) }},
	{"composite/ephemeral", func(t *testing.T) Manager { return newContractComposite(t, ephemeralTTL) }},
	{"composite/long-lived", func(t *testing.T) Manager { return newContractComposite(t, time.Minute) }},
}

var longLivedStores = []contractStore[fullManager]{
	{"sqlite", func(t *testing.T) fullManager { return newTestSQLiteWithID(t, 1024, perfIDs()) }},
	{"composite", func(t *testing.T) fullManager { return newContractComposite(t, ephemeralTTL) }},
}

func newContractComposite(t *testing.T, threshold time.Duration) *CompositeManager {
	idGen := perfIDs()
	return NewCompositeManager(NewMemoryManager(idGen), newTestSQLiteWithID(t, 1024, idGen), threshold)
}

func forEachStore(t *testing.T, run func(t *testing.T, m Manager)) {
	for _, s := range managerStores {
		t.Run(s.name, func(t *testing.T) { run(t, s.open(t)) })
	}
}

func forEachLongLivedStore(t *testing.T, run func(t *testing.T, m fullManager)) {
	for _, s := range longLivedStores {
		t.Run(s.name, func(t *testing.T) { run(t, s.open(t)) })
	}
}

// addDNS records n DNS interactions and returns them with their assigned seqs.
func addDNS(m Manager, hookID string, n int) []*Interaction {
	out := make([]*Interaction, n)
	for i := range out {
		out[i] = DNSInteraction(fmt.Sprintf("%s-i%d", hookID, i), "1.2.3.4", "q", "A")
		m.AddInteraction(hookID, out[i])
	}
	return out
}

// sameJSON compares values as they would reach an API client, which hides the
// map[string]string -> map[string]any change of a JSON round-trip.
func sameJSON(t *testing.T, got, want any) bool {
	t.Helper()
	g, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	w, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	return string(g) == string(w)
}

func TestContract_HookRoundTrip(t *testing.T) {
	forEachStore(t, func(t *testing.T, m Manager) {
		meta := map[string]any{"target": "acme", "n": float64(3), "ok": true}
		created := m.CreateHook("example.com", CreateOptions{TTL: time.Hour, Metadata: meta, SMTPEnabled: true})

		if !m.Has(created.ID) {
			t.Fatal("expected Has to report the new hook")
		}
		got, ok := m.GetHook(created.ID)
		if !ok {
			t.Fatal("expected GetHook to find the new hook")
		}
		if got.ID != created.ID || got.DNS != created.DNS || got.HTTP != created.HTTP ||
			got.HTTPS != created.HTTPS || got.SMTP != created.SMTP {
			t.Errorf("addresses changed: got %+v, created %+v", got, created)
		}
		if created.SMTP != created.ID+"@example.com" {
			t.Errorf("unexpected SMTP address %q", created.SMTP)
		}
		if !got.CreatedAt.Equal(created.CreatedAt) || !got.ExpiresAt.Equal(created.ExpiresAt) {
			t.Errorf("times changed: got %s/%s, created %s/%s", got.CreatedAt, got.ExpiresAt, created.CreatedAt, created.ExpiresAt)
		}
		if d := created.ExpiresAt.Sub(created.CreatedAt); d != time.Hour {
			t.Errorf("expected ExpiresAt = CreatedAt + TTL, got +%s", d)
		}
		if !sameJSON(t, got.Metadata, meta) {
			t.Errorf("metadata changed: got %v, want %v", got.Metadata, meta)
		}

		noTTL := m.CreateHook("example.com", CreateOptions{})
		if got, _ := m.GetHook(noTTL.ID); got == nil || !got.ExpiresAt.IsZero() || got.SMTP != "" {
			t.Errorf("expected no expiry and no SMTP address, got %+v", got)
		}
	})
}

func TestContract_UnknownHook(t *testing.T) {
	forEachStore(t, func(t *testing.T, m Manager) {
		if m.Has("missing") {
			t.Error("Has reported an unknown hook")
		}
		if _, ok := m.GetHook("missing"); ok {
			t.Error("GetHook found an unknown hook")
		}

		it := DNSInteraction("x", "1.2.3.4", "q", "A")
		m.AddInteraction("missing", it)
		if it.Seq != 0 {
			t.Errorf("a dropped interaction must not get a seq, got %d", it.Seq)
		}
		if got := m.PollInteractions("missing"); len(got) != 0 {
			t.Errorf("expected nothing to poll, got %d", len(got))
		}
		if res := m.PollInteractionsBatch([]string{"missing"})["missing"]; res == nil || res.Error != "Hook not found" {
			t.Errorf("expected a not-found batch result, got %+v", res)
		}
		if _, err := m.ReadInteractions("missing", 0); !errors.Is(err, ErrHookNotFound) {
			t.Errorf("expected ErrHookNotFound on read, got %v", err)
		}
		if _, err := m.AckInteractions("missing", 1); !errors.Is(err, ErrHookNotFound) {
			t.Errorf("expected ErrHookNotFound on ack, got %v", err)
		}
		if s := m.Stats(); s.HooksActive != 0 || s.InteractionsTotal != 0 {
			t.Errorf("unknown hooks must not show in stats, got %+v", s)
		}
	})
}

func TestContract_InteractionRoundTrip(t *testing.T) {
	forEachStore(t, func(t *testing.T, m Manager) {
		hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
		want := []*Interaction{
			DNSInteraction("dns", "10.0.0.1", hook.DNS+".", "TXT"),
			HTTPInteraction("http", "10.0.0.2", "POST", "/cb?x=1", map[string]string{"X-Test": "a, b"}, "payload"),
			SMTPInteraction("smtp", "10.0.0.3", "mx.test", "a@b.test", hook.SMTP, "tag", "Hello", "Subject: Hello\r\n\r\nbody"),
		}
		for _, it := range want {
			m.AddInteraction(hook.ID, it)
		}

		read, err := m.ReadInteractions(hook.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(read.Interactions) != len(want) {
			t.Fatalf("expected %d interactions, got %d", len(want), len(read.Interactions))
		}
		for i, got := range read.Interactions {
			w := want[i]
			if got.ID != w.ID || got.Seq != int64(i+1) || got.Type != w.Type || got.SourceIP != w.SourceIP ||
				!got.Timestamp.Equal(w.Timestamp) {
				t.Errorf("interaction %d changed: got %+v, want %+v", i, got, w)
			}
			if !sameJSON(t, got.Data, w.Data) {
				t.Errorf("interaction %d data changed: got %v, want %v", i, got.Data, w.Data)
			}
		}
	})
}

func TestContract_SeqIsPerHookAndNeverReused(t *testing.T) {
	forEachStore(t, func(t *testing.T, m Manager) {
		a := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
		b := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})

		if got := seqs(addDNS(m, a.ID, 3)); fmt.Sprint(got) != "[1 2 3]" {
			t.Errorf("expected seqs [1 2 3] on the first hook, got %v", got)
		}
		if got := seqs(addDNS(m, b.ID, 1)); fmt.Sprint(got) != "[1]" {
			t.Errorf("expected each hook to count from 1, got %v", got)
		}

		// Neither a destructive poll nor an ack may rewind the counter: a client
		// cursor past the old seqs would then miss new interactions.
		m.PollInteractions(a.ID)
		if got := seqs(addDNS(m, a.ID, 1)); fmt.Sprint(got) != "[4]" {
			t.Errorf("expected seq 4 after a poll, got %v", got)
		}
		m.AckInteractions(a.ID, 4)
		if got := seqs(addDNS(m, a.ID, 1)); fmt.Sprint(got) != "[5]" {
			t.Errorf("expected seq 5 after an ack, got %v", got)
		}
	})
}

func TestContract_PollDrainsInOrder(t *testing.T) {
	forEachStore(t, func(t *testing.T, m Manager) {
		hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
		addDNS(m, hook.ID, 3)

		if got := seqs(m.PollInteractions(hook.ID)); fmt.Sprint(got) != "[1 2 3]" {
			t.Errorf("expected seqs [1 2 3], got %v", got)
		}
		if got := m.PollInteractions(hook.ID); len(got) != 0 {
			t.Errorf("expected a second poll to be empty, got %d", len(got))
		}
		if !m.Has(hook.ID) {
			t.Error("polling must not delete the hook")
		}
	})
}

func TestContract_PollBatch(t *testing.T) {
	forEachStore(t, func(t *testing.T, m Manager) {
		fired := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
		quiet := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
		addDNS(m, fired.ID, 2)

		res := m.PollInteractionsBatch([]string{fired.ID, quiet.ID, "missing", fired.ID})
		if len(res) != 3 {
			t.Errorf("expected one result per distinct id, got %d", len(res))
		}
		if got := res[fired.ID]; got == nil || got.Error != "" || fmt.Sprint(seqs(got.Interactions)) != "[1 2]" {
			t.Errorf("a duplicate id must not replace the first drain, got %+v", got)
		}
		if got := res[quiet.ID]; got == nil || got.Error != "" || got.Interactions == nil || len(got.Interactions) != 0 {
			t.Errorf("expected an empty, non-nil result for a quiet hook, got %+v", got)
		}
		if got := res["missing"]; got == nil || got.Error != "Hook not found" {
			t.Errorf("expected a not-found result, got %+v", got)
		}
		if got := m.PollInteractions(fired.ID); len(got) != 0 {
			t.Errorf("expected the batch to have drained the hook, got %d left", len(got))
		}
	})
}

func TestContract_StatsTrackEveryRemoval(t *testing.T) {
	forEachStore(t, func(t *testing.T, m Manager) {
		polled := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
		acked := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
		m.AddInteraction(polled.ID, DNSInteraction("d", "1.2.3.4", "q", "A"))
		m.AddInteraction(polled.ID, HTTPInteraction("h", "1.2.3.4", "GET", "/", nil, ""))
		m.AddInteraction(acked.ID, SMTPInteraction("s", "1.2.3.4", "helo", "a@b", "c@d", "", "", "body"))
		m.AddInteraction(acked.ID, DNSInteraction("d2", "1.2.3.4", "q", "A"))

		want := func(step string, hooks, total, dns, http, smtp int) {
			t.Helper()
			s := m.Stats()
			if s.HooksActive != hooks || s.InteractionsTotal != total || s.InteractionsDNS != dns ||
				s.InteractionsHTTP != http || s.InteractionsSMTP != smtp {
				t.Errorf("%s: got hooks=%d total=%d dns=%d http=%d smtp=%d, want %d/%d/%d/%d/%d", step,
					s.HooksActive, s.InteractionsTotal, s.InteractionsDNS, s.InteractionsHTTP, s.InteractionsSMTP,
					hooks, total, dns, http, smtp)
			}
		}
		want("after captures", 2, 4, 2, 1, 1)
		m.PollInteractions(polled.ID)
		want("after poll", 2, 2, 1, 0, 1)
		m.AckInteractions(acked.ID, 1)
		want("after ack", 2, 1, 1, 0, 0)
		m.EvictExpiredHooks(time.Now().Add(2 * time.Hour))
		want("after hook expiry", 0, 0, 0, 0, 0)
	})
}

func TestContract_EvictExpiredHooks(t *testing.T) {
	forEachStore(t, func(t *testing.T, m Manager) {
		short := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
		long := m.CreateHook("example.com", CreateOptions{TTL: 3 * time.Hour})
		forever := m.CreateHook("example.com", CreateOptions{})
		addDNS(m, short.ID, 2)

		if n := m.EvictExpiredHooks(time.Now()); n != 0 {
			t.Errorf("nothing has expired yet, evicted %d", n)
		}
		if n := m.EvictExpiredHooks(time.Now().Add(2 * time.Hour)); n != 1 {
			t.Errorf("expected 1 hook evicted, got %d", n)
		}
		if m.Has(short.ID) {
			t.Error("expired hook is still known")
		}
		if _, err := m.ReadInteractions(short.ID, 0); !errors.Is(err, ErrHookNotFound) {
			t.Errorf("expired hook's interactions must go with it, got %v", err)
		}
		if !m.Has(long.ID) || !m.Has(forever.ID) {
			t.Error("unexpired hooks and hooks without expiry must be kept")
		}
		if n := m.EvictExpiredHooks(time.Now().Add(100 * 365 * 24 * time.Hour)); n != 1 || !m.Has(forever.ID) {
			t.Errorf("a zero ExpiresAt never expires, evicted %d", n)
		}
	})
}

func TestContract_PerHookLimitReportsLoss(t *testing.T) {
	forEachStore(t, func(t *testing.T, m Manager) {
		hook := m.CreateHook("example.com", CreateOptions{TTL: time.Hour})
		addDNS(m, hook.ID, 10)

		if removed := m.EnforcePerHookLimit(4); removed != 6 {
			t.Fatalf("expected 6 removed, got %d", removed)
		}
		if removed := m.EnforcePerHookLimit(4); removed != 0 {
			t.Errorf("a second pass has nothing to trim, got %d", removed)
		}
		read, _ := m.ReadInteractions(hook.ID, 0)
		if read.DroppedThrough != 6 {
			t.Errorf("expected dropped_through 6, got %d", read.DroppedThrough)
		}
		if got := seqs(read.Interactions); fmt.Sprint(got) != "[7 8 9 10]" {
			t.Errorf("expected newest seqs kept, got %v", got)
		}
	})
}

func TestContract_ConcurrentCaptureReadAndAck(t *testing.T) {
	forEachStore(t, func(t *testing.T, store Manager) {
		const writers, perWriter = 4, 100
		id := store.CreateHook("example.test", CreateOptions{TTL: time.Hour}).ID
		var wg sync.WaitGroup
		for writer := 0; writer < writers; writer++ {
			wg.Add(1)
			go func(writer int) {
				defer wg.Done()
				for i := 0; i < perWriter; i++ {
					it := &Interaction{ID: fmt.Sprintf("%d-%d", writer, i), Type: InteractionTypeDNS}
					store.AddInteraction(id, it)
					if it.Seq == 0 {
						t.Error("capture lost")
					}
				}
			}(writer)
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()

		var cursor int64
		deadline := time.Now().Add(10 * time.Second)
		for {
			if time.Now().After(deadline) {
				t.Fatal("concurrent drain timed out")
			}
			read, err := store.ReadInteractions(id, cursor)
			if err != nil {
				t.Fatal(err)
			}
			for _, it := range read.Interactions {
				if it.Seq != cursor+1 {
					t.Fatalf("sequence gap: %d -> %d", cursor, it.Seq)
				}
				cursor = it.Seq
			}
			if _, err := store.AckInteractions(id, cursor); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
				if cursor == writers*perWriter {
					if total := store.Stats().InteractionsTotal; total != 0 {
						t.Fatalf("expected an empty store after the drain, got %d", total)
					}
					return
				}
			default:
			}
			if cursor > writers*perWriter {
				t.Fatal("extra interaction")
			}
		}
	})
}

func TestContract_LongLivedCap(t *testing.T) {
	forEachLongLivedStore(t, func(t *testing.T, m fullManager) {
		const max, attempts = 5, 20
		opts := CreateOptions{TTL: 2 * ephemeralTTL}

		var wg sync.WaitGroup
		var mu sync.Mutex
		created, refused := 0, 0
		for i := 0; i < attempts; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := m.CreateLongLivedHook("example.com", opts, max)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					created++
				case errors.Is(err, ErrHookLimitReached):
					refused++
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		wg.Wait()

		if created != max || refused != attempts-max {
			t.Errorf("expected %d created and %d refused, got %d and %d", max, attempts-max, created, refused)
		}
		if n := m.LongLivedCount(); n != max {
			t.Errorf("expected count %d, got %d", max, n)
		}
	})
}

func TestContract_LongLivedBatchIsAllOrNothing(t *testing.T) {
	forEachLongLivedStore(t, func(t *testing.T, m fullManager) {
		opts := CreateOptions{TTL: 2 * ephemeralTTL}
		if _, err := m.CreateLongLivedHook("example.com", opts, 3); err != nil {
			t.Fatal(err)
		}
		if _, err := m.CreateLongLivedHooks("example.com", []CreateOptions{opts, opts, opts}, 3); !errors.Is(err, ErrHookLimitReached) {
			t.Fatalf("expected ErrHookLimitReached, got %v", err)
		}
		if n := m.LongLivedCount(); n != 1 {
			t.Errorf("a refused batch must create nothing, count is %d", n)
		}

		hooks, err := m.CreateLongLivedHooks("example.com", []CreateOptions{opts, opts}, 3)
		if err != nil || len(hooks) != 2 {
			t.Fatalf("expected a batch of 2, got %d (%v)", len(hooks), err)
		}
		for _, h := range hooks {
			if !m.Has(h.ID) {
				t.Errorf("batch hook %s is not known", h.ID)
			}
		}
	})
}

func TestContract_LongLivedHooksOldestFirst(t *testing.T) {
	forEachLongLivedStore(t, func(t *testing.T, m fullManager) {
		var want []string
		for i := 0; i < 3; i++ {
			h, err := m.CreateLongLivedHook("example.com", CreateOptions{TTL: 2 * ephemeralTTL}, 0)
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, h.ID)
		}
		hooks, err := m.LongLivedHooks()
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, len(hooks))
		for i, h := range hooks {
			got[i] = h.ID
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("expected %v oldest first, got %v", want, got)
		}
	})
}

func TestContract_LongLivedActivity(t *testing.T) {
	forEachLongLivedStore(t, func(t *testing.T, m fullManager) {
		opts := CreateOptions{TTL: 2 * ephemeralTTL, Metadata: map[string]any{"n": "1"}}
		older, _ := m.CreateLongLivedHook("example.com", opts, 0)
		newer, _ := m.CreateLongLivedHook("example.com", opts, 0)
		quiet, _ := m.CreateLongLivedHook("example.com", opts, 0)

		at := time.Now().UTC().Add(-time.Minute)
		for i, id := range []string{older.ID, older.ID, newer.ID} {
			it := DNSInteraction(fmt.Sprintf("i%d", i), "1.2.3.4", "q", "A")
			it.Timestamp = at.Add(time.Duration(i) * time.Second)
			m.AddInteraction(id, it)
		}

		activity := m.LongLivedActivity()
		if len(activity) != 2 {
			t.Fatalf("expected 2 active hooks, got %d", len(activity))
		}
		first, second := activity[0], activity[1]
		if first.Hook.ID != newer.ID || second.Hook.ID != older.ID {
			t.Errorf("expected most recent activity first, got %s then %s", first.Hook.ID, second.Hook.ID)
		}
		if second.PendingCount != 2 || second.LastSeq != 2 || !second.LastInteractionAt.Equal(at.Add(time.Second)) {
			t.Errorf("unexpected activity for the older hook: %+v", second)
		}
		if second.Hook.Metadata["n"] != "1" {
			t.Errorf("expected metadata in activity, got %v", second.Hook.Metadata)
		}
		for _, a := range activity {
			if a.Hook.ID == quiet.ID {
				t.Error("a hook without pending interactions is not active")
			}
		}

		m.AckInteractions(older.ID, 2)
		if activity := m.LongLivedActivity(); len(activity) != 1 || activity[0].Hook.ID != newer.ID {
			t.Errorf("an acknowledged hook is no longer active, got %+v", activity)
		}
	})
}
