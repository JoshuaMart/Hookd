package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/credential"
	"github.com/jomar/hookd/internal/storage"
)

const (
	secretA = "0123456789abcdef0123456789abcdef"
	secretB = "fedcba9876543210fedcba9876543210"
)

// newOwnerHandler is newLongLivedHandler with owner secrets on and a log sink.
func newOwnerHandler(t *testing.T) (*APIHandler, *storage.CompositeManager, *bytes.Buffer) {
	t.Helper()
	return newOwnerHandlerWith(t, func(*storage.SQLiteManager) {})
}

func newOwnerHandlerWith(t *testing.T, configure func(*storage.SQLiteManager)) (*APIHandler, *storage.CompositeManager, *bytes.Buffer) {
	t.Helper()
	h, store := newLongLivedHandlerWith(t, configure)
	logs := &bytes.Buffer{}
	h.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.ownerSecret = true
	return h, store, logs
}

// ownerRequest sends one request through the API routes, without auth.
func ownerRequest(h *APIHandler, method, target, body, secret string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc("/register", h.HandleRegister)
	mux.HandleFunc("/poll", h.HandlePollBatch)
	mux.HandleFunc("/poll/", h.HandlePoll)
	mux.HandleFunc("/read", h.HandleRead)
	mux.HandleFunc("/ack", h.HandleAck)
	mux.HandleFunc("/activity", h.HandleActivity)
	mux.HandleFunc("/hooks", h.HandleHooks)

	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if secret != "" {
		req.Header.Set(ownerSecretHeader, secret)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// registerOwned registers one hook and returns its id and the response body.
func registerOwned(t *testing.T, h *APIHandler, body, secret string) (string, map[string]any) {
	t.Helper()
	w := ownerRequest(h, http.MethodPost, "/register", body, secret)
	if w.Code != http.StatusOK {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	id, _ := resp["id"].(string)
	return id, resp
}

func TestOwner_DisabledKeepsTheAPIUnchanged(t *testing.T) {
	h, store := newLongLivedHandler(t)

	// The header is ignored, even when it would be invalid.
	id, resp := registerOwned(t, h, `{"ttl":"48h"}`, "short")
	if _, ok := resp["secret"]; ok {
		t.Errorf("no secret must be returned while disabled: %v", resp)
	}
	if hook, _ := store.GetHook(id); hook.Owner != "" {
		t.Errorf("no owner must be stored while disabled, got %q", hook.Owner)
	}
	w := ownerRequest(h, http.MethodPost, "/register", `{"count":2}`, "")
	if strings.Contains(w.Body.String(), "secret") || w.Header().Get("Cache-Control") != "" {
		t.Errorf("unexpected register response: %v %s", w.Header(), w.Body.String())
	}
	for _, target := range []string{"/poll/" + id, "/hooks", "/activity"} {
		if w := ownerRequest(h, http.MethodGet, target, "", ""); w.Code != http.StatusOK {
			t.Errorf("GET %s without secret: %d", target, w.Code)
		}
	}
}

func TestOwner_GeneratedSecret(t *testing.T) {
	h, _, _ := newOwnerHandler(t)

	w := ownerRequest(h, http.MethodPost, "/register", "", "")
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	secret, _ := resp["secret"].(string)
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(secret) {
		t.Fatalf("expected a 256-bit hex secret, got %q", secret)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("a response carrying a secret must not be cached")
	}
	id, _ := resp["id"].(string)
	if id == "" || resp["dns"] == nil {
		t.Errorf("the hook fields must stay at the top level: %v", resp)
	}
	if w := ownerRequest(h, http.MethodGet, "/poll/"+id, "", secret); w.Code != http.StatusOK {
		t.Errorf("poll with the generated secret: %d %s", w.Code, w.Body.String())
	}

	// A list response carries one secret for every hook.
	w = ownerRequest(h, http.MethodPost, "/register", `{"count":2}`, "")
	var list struct {
		Hooks  []storage.Hook `json:"hooks"`
		Secret string         `json:"secret"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Hooks) != 2 || list.Secret == "" || list.Secret == secret {
		t.Fatalf("unexpected list response: %s", w.Body.String())
	}
	for _, hook := range list.Hooks {
		if w := ownerRequest(h, http.MethodGet, "/poll/"+hook.ID, "", list.Secret); w.Code != http.StatusOK {
			t.Errorf("poll %s: %d", hook.ID, w.Code)
		}
		if w := ownerRequest(h, http.MethodGet, "/poll/"+hook.ID, "", secret); w.Code != http.StatusNotFound {
			t.Errorf("each register gets its own owner, got %d", w.Code)
		}
	}
}

func TestOwner_ProvidedSecret(t *testing.T) {
	h, _, _ := newOwnerHandler(t)

	id, resp := registerOwned(t, h, `{"hooks":[{},{"ttl":"48h"}]}`, secretA)
	if _, ok := resp["secret"]; ok {
		t.Error("a client secret must not be echoed")
	}
	hooks, _ := resp["hooks"].([]any)
	if id != "" || len(hooks) != 2 {
		t.Fatalf("unexpected response: %v", resp)
	}
	for _, hook := range hooks {
		id := hook.(map[string]any)["id"].(string)
		if w := ownerRequest(h, http.MethodGet, "/poll/"+id, "", secretA); w.Code != http.StatusOK {
			t.Errorf("poll %s with its secret: %d", id, w.Code)
		}
	}
}

func TestOwner_InvalidSecretIsRejected(t *testing.T) {
	h, _, _ := newOwnerHandler(t)
	for _, secret := range []string{
		secretA[:31],
		strings.Repeat("a", 257),
		"0123456789abcdef 0123456789abcdef",
		"0123456789abcdefé0123456789abcdef",
	} {
		for _, target := range []string{"/register", "/poll"} {
			w := ownerRequest(h, http.MethodPost, target, `["x"]`, secret)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), ownerSecretHeader) {
				t.Errorf("POST %s with %q: %d %s", target, secret, w.Code, w.Body.String())
			}
		}
	}
	if !validSecret(strings.Repeat("~", 256)) {
		t.Error("256 visible characters must be accepted")
	}
}

func TestOwner_MissingSecretIsRefused(t *testing.T) {
	h, _, _ := newOwnerHandler(t)
	for _, tc := range []struct{ method, target, body string }{
		{http.MethodGet, "/poll/x", ""},
		{http.MethodGet, "/poll/x?after=0", ""},
		{http.MethodDelete, "/poll/x?through=1", ""},
		{http.MethodPost, "/poll", `["x"]`},
		{http.MethodPost, "/read", `{"after":{"x":0}}`},
		{http.MethodPost, "/ack", `{"through":{"x":1}}`},
		{http.MethodGet, "/hooks", ""},
		{http.MethodGet, "/activity", ""},
	} {
		w := ownerRequest(h, tc.method, tc.target, tc.body, "")
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "owner secret") {
			t.Errorf("%s %s without secret: %d %s", tc.method, tc.target, w.Code, w.Body.String())
		}
	}
	// The secret is checked before the body, as documented.
	for _, target := range []string{"/poll", "/read", "/ack"} {
		if w := ownerRequest(h, http.MethodPost, target, "not json", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("POST %s with a bad body and no secret: %d", target, w.Code)
		}
	}
}

func TestOwner_PerOwnerLongLivedCap(t *testing.T) {
	h, _, _ := newOwnerHandlerWith(t, func(s *storage.SQLiteManager) { s.SetMaxPerOwner(1) })
	registerOwned(t, h, `{"ttl":"48h"}`, secretA)

	w := ownerRequest(h, http.MethodPost, "/register", `{"ttl":"48h"}`, secretA)
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "for this owner") {
		t.Errorf("second hook of A: %d %s", w.Code, w.Body.String())
	}
	registerOwned(t, h, `{"ttl":"48h"}`, secretB)
	if w := ownerRequest(h, http.MethodPost, "/register", "", secretA); w.Code != http.StatusOK {
		t.Errorf("ephemeral hooks are not capped per owner: %d", w.Code)
	}
}

func TestOwner_SameSecretGroupsHooks(t *testing.T) {
	h, store, _ := newOwnerHandler(t)
	a1, _ := registerOwned(t, h, `{"ttl":"48h","metadata":{"run":"1"}}`, secretA)
	a2, _ := registerOwned(t, h, `{"ttl":"48h","metadata":{"run":"2"}}`, secretA)
	b1, _ := registerOwned(t, h, `{"ttl":"48h","metadata":{"run":"1"}}`, secretB)
	for _, id := range []string{a1, a2, b1} {
		store.AddInteraction(id, storage.DNSInteraction("i-"+id, "1.2.3.4", "q", "A"))
	}

	listed := func(target, secret string) string {
		w := ownerRequest(h, http.MethodGet, target, "", secret)
		var resp struct {
			Hooks []json.RawMessage `json:"hooks"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		var ids []string
		for _, raw := range resp.Hooks {
			var item struct {
				ID   string `json:"id"`
				Hook struct {
					ID string `json:"id"`
				} `json:"hook"`
			}
			_ = json.Unmarshal(raw, &item)
			ids = append(ids, item.ID+item.Hook.ID)
		}
		return strings.Join(ids, ",")
	}
	for _, tc := range []struct{ target, secret, want string }{
		{"/hooks", secretA, a1 + "," + a2},
		{"/hooks", secretB, b1},
		{"/hooks?metadata.run=2", secretA, a2},
		{"/hooks?metadata.run=2", secretB, ""},
		{"/activity?metadata.run=1", secretA, a1},
		{"/activity?metadata.run=1", secretB, b1},
	} {
		if got := listed(tc.target, tc.secret); got != tc.want {
			t.Errorf("GET %s: got %q, want %q", tc.target, got, tc.want)
		}
	}
	if got := listed("/activity", secretA); !strings.Contains(got, a1) || !strings.Contains(got, a2) || strings.Contains(got, b1) {
		t.Errorf("activity of A: %q", got)
	}
}

func TestOwner_ForeignHookMatchesUnknown(t *testing.T) {
	for _, ttl := range []string{"", `"ttl":"48h"`} {
		t.Run(fmt.Sprintf("ttl=%q", ttl), func(t *testing.T) {
			h, store, _ := newOwnerHandler(t)
			id, _ := registerOwned(t, h, "{"+ttl+"}", secretA)
			store.AddInteraction(id, storage.DNSInteraction("i1", "1.2.3.4", "q", "A"))
			const unknown = "unknown"

			for _, tc := range []struct{ method, target, body string }{
				{http.MethodGet, "/poll/%s", ""},
				{http.MethodGet, "/poll/%s?after=0", ""},
				{http.MethodDelete, "/poll/%s?through=5", ""},
				{http.MethodPost, "/poll", `["%s"]`},
				{http.MethodPost, "/read", `{"after":{"%s":0}}`},
				{http.MethodPost, "/ack", `{"through":{"%s":5}}`},
			} {
				send := func(target string) (int, string) {
					path, body := tc.target, tc.body
					if strings.Contains(path, "%s") {
						path = fmt.Sprintf(path, target)
					} else {
						body = fmt.Sprintf(body, target)
					}
					w := ownerRequest(h, tc.method, path, body, secretB)
					return w.Code, strings.ReplaceAll(w.Body.String(), target, "ID")
				}
				foreignCode, foreignBody := send(id)
				unknownCode, unknownBody := send(unknown)
				if foreignCode != unknownCode || foreignBody != unknownBody {
					t.Errorf("%s %s: foreign %d %s, unknown %d %s",
						tc.method, tc.target, foreignCode, foreignBody, unknownCode, unknownBody)
				}
			}

			// Nothing was drained or acknowledged on the owner's behalf.
			w := ownerRequest(h, http.MethodGet, "/poll/"+id+"?after=0", "", secretA)
			if !strings.Contains(w.Body.String(), `"seq":1`) {
				t.Errorf("owner lost its interaction: %s", w.Body.String())
			}
		})
	}
}

func TestOwner_BatchPollMixesOwners(t *testing.T) {
	h, store, _ := newOwnerHandler(t)
	mine, _ := registerOwned(t, h, `{"ttl":"48h"}`, secretA)
	theirs, _ := registerOwned(t, h, `{"ttl":"48h"}`, secretB)
	ephemeral, _ := registerOwned(t, h, "", secretB)
	for _, id := range []string{mine, theirs, ephemeral} {
		store.AddInteraction(id, storage.DNSInteraction("i-"+id, "1.2.3.4", "q", "A"))
	}

	w := ownerRequest(h, http.MethodPost, "/poll", fmt.Sprintf(`[%q,%q,%q,"unknown"]`, mine, theirs, ephemeral), secretA)
	var resp struct {
		Results map[string]json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || w.Code != http.StatusOK {
		t.Fatalf("batch poll: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(string(resp.Results[mine]), "i-"+mine) {
		t.Errorf("own hook: %s", resp.Results[mine])
	}
	for _, id := range []string{theirs, ephemeral} {
		if string(resp.Results[id]) != string(resp.Results["unknown"]) {
			t.Errorf("foreign %s: %s, unknown: %s", id, resp.Results[id], resp.Results["unknown"])
		}
	}
	if w := ownerRequest(h, http.MethodGet, "/poll/"+theirs, "", secretB); !strings.Contains(w.Body.String(), "i-"+theirs) {
		t.Errorf("the other owner's interaction must survive: %s", w.Body.String())
	}
}

func TestOwner_SecretNeverLeaks(t *testing.T) {
	h, store, logs := newOwnerHandler(t)
	_, resp := registerOwned(t, h, `{"ttl":"48h","metadata":{"k":"v"}}`, "")
	generated := resp["secret"].(string)
	id := resp["id"].(string)
	provided, _ := registerOwned(t, h, `{"ttl":"48h"}`, secretA)
	store.AddInteraction(id, storage.DNSInteraction("i1", "1.2.3.4", "q", "A"))

	var bodies strings.Builder
	for _, tc := range []struct{ method, target, body, secret string }{
		{http.MethodGet, "/hooks", "", generated},
		{http.MethodGet, "/activity", "", generated},
		{http.MethodGet, "/poll/" + id + "?after=0", "", generated},
		{http.MethodPost, "/read", fmt.Sprintf(`{"after":{%q:0}}`, id), generated},
		{http.MethodPost, "/ack", fmt.Sprintf(`{"through":{%q:1}}`, id), generated},
		{http.MethodPost, "/poll", fmt.Sprintf(`[%q]`, provided), secretA},
		{http.MethodDelete, "/poll/" + provided + "?through=1", "", secretA},
	} {
		w := ownerRequest(h, tc.method, tc.target, tc.body, tc.secret)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", tc.method, tc.target, w.Code, w.Body.String())
		}
		bodies.WriteString(w.Body.String())
	}

	for _, secret := range []string{generated, secretA} {
		owner := credential.Hash(secret)
		if strings.Contains(bodies.String(), secret) || strings.Contains(bodies.String(), owner) {
			t.Error("a response leaked the secret or its hash")
		}
		if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), owner) {
			t.Error("the logs leaked the secret or its hash")
		}
		if !strings.Contains(logs.String(), "owner="+owner[:8]) {
			t.Errorf("expected the owner fingerprint in the logs:\n%s", logs.String())
		}
	}
}

func TestOwner_ServerWiresTheOption(t *testing.T) {
	s := newObservabilityTestServer(config.ObservabilityConfig{}, config.HTTPSConfig{})
	s.config.API.OwnerSecret.Enabled = true
	handler, _ := s.newHandlers()
	if w := serveTest(handler, http.MethodGet, "example.com", "/hooks", "test-token", false); w.Code != http.StatusUnauthorized {
		t.Errorf("expected the owner secret to be required, got %d", w.Code)
	}
	// The API token is still checked first: the secret does not replace it.
	if w := serveTest(handler, http.MethodGet, "example.com", "/hooks", "", false); w.Code != http.StatusUnauthorized ||
		!strings.Contains(w.Body.String(), "API key") {
		t.Errorf("expected the API key to be required, got %d %s", w.Code, w.Body.String())
	}
}
