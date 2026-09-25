package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jomar/hookd/internal/storage"
)

type cursorResponse struct {
	Interactions   []storage.Interaction `json:"interactions"`
	DroppedThrough *int64                `json:"dropped_through"`
	Acknowledged   int                   `json:"acknowledged"`
	Error          string                `json:"error"`
}

func doPoll(t *testing.T, h *APIHandler, method, target string) (int, cursorResponse) {
	t.Helper()
	w := httptest.NewRecorder()
	h.HandlePoll(w, httptest.NewRequest(method, target, nil))
	var resp cursorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return w.Code, resp
}

func TestPoll_CursorReadAndAck(t *testing.T) {
	handler, composite := newLongLivedHandler(t)
	hook := composite.CreateHook("example.com", storage.CreateOptions{TTL: 720 * time.Hour})
	for _, id := range []string{"i1", "i2", "i3"} {
		composite.AddInteraction(hook.ID, storage.DNSInteraction(id, "1.2.3.4", "q", "A"))
	}

	code, resp := doPoll(t, handler, http.MethodGet, "/poll/"+hook.ID+"?after=1")
	if code != http.StatusOK || len(resp.Interactions) != 2 || resp.Interactions[0].Seq != 2 {
		t.Fatalf("expected seqs 2..3, got %d %+v", code, resp)
	}
	if resp.DroppedThrough == nil || *resp.DroppedThrough != 0 {
		t.Errorf("expected dropped_through 0 on a cursor read, got %v", resp.DroppedThrough)
	}

	// Reading again returns the same interactions.
	if _, again := doPoll(t, handler, http.MethodGet, "/poll/"+hook.ID+"?after=0"); len(again.Interactions) != 3 {
		t.Fatalf("expected cursor read to keep interactions, got %d", len(again.Interactions))
	}

	code, resp = doPoll(t, handler, http.MethodDelete, "/poll/"+hook.ID+"?through=2")
	if code != http.StatusOK || resp.Acknowledged != 2 {
		t.Fatalf("expected 2 acknowledged, got %d %+v", code, resp)
	}
	if _, left := doPoll(t, handler, http.MethodGet, "/poll/"+hook.ID+"?after=0"); len(left.Interactions) != 1 {
		t.Errorf("expected 1 interaction left, got %d", len(left.Interactions))
	}
}

func TestPoll_CursorValidation(t *testing.T) {
	handler, composite := newLongLivedHandler(t)
	hook := composite.CreateHook("example.com", storage.CreateOptions{TTL: 720 * time.Hour})

	for _, tc := range []struct {
		name, method, target string
		want                 int
	}{
		{"negative after", http.MethodGet, "/poll/" + hook.ID + "?after=-1", http.StatusBadRequest},
		{"non-numeric after", http.MethodGet, "/poll/" + hook.ID + "?after=abc", http.StatusBadRequest},
		{"delete without through", http.MethodDelete, "/poll/" + hook.ID, http.StatusBadRequest},
		{"delete unknown hook", http.MethodDelete, "/poll/missing?through=1", http.StatusNotFound},
		{"read unknown hook", http.MethodGet, "/poll/missing?after=0", http.StatusNotFound},
		{"empty after", http.MethodGet, "/poll/" + hook.ID + "?after=", http.StatusBadRequest},
		{"empty through", http.MethodDelete, "/poll/" + hook.ID + "?through=", http.StatusBadRequest},
		{"put", http.MethodPut, "/poll/" + hook.ID, http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, _ := doPoll(t, handler, tc.method, tc.target); code != tc.want {
				t.Errorf("expected %d, got %d", tc.want, code)
			}
		})
	}
}

func TestReadAndAckBatch(t *testing.T) {
	handler, composite := newLongLivedHandler(t)
	hook := composite.CreateHook("example.com", storage.CreateOptions{TTL: 720 * time.Hour})
	composite.AddInteraction(hook.ID, storage.DNSInteraction("i1", "1.2.3.4", "q", "A"))
	composite.AddInteraction(hook.ID, storage.DNSInteraction("i2", "1.2.3.4", "q", "A"))

	post := func(fn http.HandlerFunc, body string) (int, map[string]cursorResponse) {
		w := httptest.NewRecorder()
		fn(w, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body)))
		var resp struct {
			Results map[string]cursorResponse `json:"results"`
		}
		_ = json.NewDecoder(w.Body).Decode(&resp)
		return w.Code, resp.Results
	}

	code, results := post(handler.HandleRead, `{"after":{"`+hook.ID+`":1,"missing":0}}`)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if got := results[hook.ID]; len(got.Interactions) != 1 || got.Interactions[0].Seq != 2 || got.DroppedThrough == nil {
		t.Errorf("unexpected cursor batch result %+v", got)
	}
	if results["missing"].Error == "" {
		t.Error("expected an error for an unknown hook")
	}

	code, results = post(handler.HandleAck, `{"through":{"`+hook.ID+`":2}}`)
	if code != http.StatusOK || results[hook.ID].Acknowledged != 2 {
		t.Fatalf("expected 2 acknowledged, got %d %+v", code, results)
	}

	if code, _ := post(handler.HandleAck, `{"through":{}}`); code != http.StatusBadRequest {
		t.Errorf("expected 400 for an empty ack, got %d", code)
	}
	if code, _ := post(handler.HandleRead, `{"after":{"`+hook.ID+`":-1}}`); code != http.StatusBadRequest {
		t.Errorf("expected 400 for a negative cursor, got %d", code)
	}
	if code, _ := post(handler.HandleRead, `{"through":{"`+hook.ID+`":1}}`); code != http.StatusBadRequest {
		t.Errorf("expected 400 for the wrong field, got %d", code)
	}

	// The legacy array form still drains.
	composite.AddInteraction(hook.ID, storage.DNSInteraction("i3", "1.2.3.4", "q", "A"))
	if _, results = post(handler.HandlePollBatch, `["`+hook.ID+`"]`); len(results[hook.ID].Interactions) != 1 {
		t.Fatalf("expected legacy drain to return 1, got %+v", results)
	}
	if read, _ := composite.ReadInteractions(hook.ID, 0); len(read.Interactions) != 0 {
		t.Errorf("expected legacy drain to empty the hook, got %d", len(read.Interactions))
	}
}
