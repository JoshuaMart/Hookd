package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jomar/hookd/internal/storage"
)

func TestRegister_HookSpecs(t *testing.T) {
	handler, composite := newLongLivedHandler(t)

	w := registerBody(t, handler, `{"hooks":[
		{"ttl":"7d","metadata":{"param":"bio"}},
		{"metadata":{"param":"name"}},
		{"ttl":"7d","metadata":{"param":"email"}}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Hooks []storage.Hook `json:"hooks"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Hooks) != 3 {
		t.Fatalf("expected 3 hooks, got %d", len(resp.Hooks))
	}
	for i, want := range []string{"bio", "name", "email"} {
		if resp.Hooks[i].Metadata["param"] != want {
			t.Errorf("hooks[%d]: expected param %q, got %v", i, want, resp.Hooks[i].Metadata)
		}
	}
	if resp.Hooks[1].ExpiresAt.After(time.Now().Add(testEphemeralTTL + time.Minute)) {
		t.Error("expected the entry without ttl to be ephemeral")
	}
	if n := composite.LongLivedCount(); n != 2 {
		t.Errorf("expected 2 long-lived hooks, got %d", n)
	}
}

func TestRegister_HookSpecsSingleEntryStillArray(t *testing.T) {
	handler, _ := newLongLivedHandler(t)
	w := registerBody(t, handler, `{"hooks":[{"metadata":{"k":"v"}}]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"hooks"`) {
		t.Fatalf("expected a hooks array, got %d %s", w.Code, w.Body.String())
	}
}

func TestRegister_HookSpecsRejected(t *testing.T) {
	handler, composite := newLongLivedHandler(t)

	for _, tc := range []struct {
		name, body string
		want       int
		contains   string
	}{
		{"empty", `{"hooks":[]}`, http.StatusBadRequest, "1 to"},
		{"combined with count", `{"count":2,"hooks":[{}]}`, http.StatusBadRequest, "cannot be combined"},
		{"invalid entry", `{"hooks":[{"ttl":"7d"},{"ttl":"bogus"}]}`, http.StatusBadRequest, "hooks[1]"},
		// The handler caps long-lived hooks at 3.
		{"over cap", `{"hooks":[{"ttl":"7d"},{"ttl":"7d"},{"ttl":"7d"},{"ttl":"7d"}]}`, http.StatusTooManyRequests, "limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := registerBody(t, handler, tc.body)
			if w.Code != tc.want || !strings.Contains(w.Body.String(), tc.contains) {
				t.Errorf("expected %d containing %q, got %d %s", tc.want, tc.contains, w.Code, w.Body.String())
			}
		})
	}
	if n := composite.LongLivedCount(); n != 0 {
		t.Errorf("expected rejected batches to create nothing, got %d", n)
	}

	var specs []string
	for range maxRegisterSpecs + 1 {
		specs = append(specs, "{}")
	}
	if w := registerBody(t, handler, `{"hooks":[`+strings.Join(specs, ",")+`]}`); w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 above %d entries, got %d", maxRegisterSpecs, w.Code)
	}
}

func getJSON(t *testing.T, fn http.HandlerFunc, target string) (int, []map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	fn(w, httptest.NewRequest(http.MethodGet, target, nil))
	var resp struct {
		Hooks []map[string]any `json:"hooks"`
	}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	return w.Code, resp.Hooks
}

func TestActivityAndHooks_MetadataFilter(t *testing.T) {
	handler, composite := newLongLivedHandler(t)
	mine := composite.CreateHook("example.com", storage.CreateOptions{TTL: 720 * time.Hour, Metadata: map[string]any{"run_id": "0f3a"}})
	other := composite.CreateHook("example.com", storage.CreateOptions{TTL: 720 * time.Hour, Metadata: map[string]any{"run_id": "9b21"}})
	composite.AddInteraction(mine.ID, storage.DNSInteraction("i1", "1.2.3.4", "q", "A"))
	composite.AddInteraction(other.ID, storage.DNSInteraction("i2", "1.2.3.4", "q", "A"))

	if _, all := getJSON(t, handler.HandleActivity, "/activity"); len(all) != 2 {
		t.Fatalf("expected 2 active hooks unfiltered, got %d", len(all))
	}
	_, got := getJSON(t, handler.HandleActivity, "/activity?metadata.run_id=0f3a")
	if len(got) != 1 || got[0]["hook"].(map[string]any)["id"] != mine.ID {
		t.Errorf("expected only the matching hook, got %v", got)
	}

	code, hooks := getJSON(t, handler.HandleHooks, "/hooks?metadata.run_id=9b21")
	if code != http.StatusOK || len(hooks) != 1 || hooks[0]["id"] != other.ID {
		t.Errorf("expected only the other hook, got %d %v", code, hooks)
	}
	if _, none := getJSON(t, handler.HandleHooks, "/hooks?metadata.run_id=nope"); none == nil || len(none) != 0 {
		t.Errorf("expected an empty array, got %v", none)
	}
	if _, all := getJSON(t, handler.HandleHooks, "/hooks"); len(all) != 2 {
		t.Errorf("expected both hooks unfiltered, got %d", len(all))
	}
}
