package hookd

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func setupServer(handler http.HandlerFunc) (*httptest.Server, *Client) {
	server := httptest.NewServer(handler)
	client := NewClient(server.URL, "test-token")
	return server, client
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("failed to encode JSON response: %v", err)
	}
}

func TestNewClient(t *testing.T) {
	client := NewClient("https://hookd.example.com", "my-token")
	if client.server != "https://hookd.example.com" {
		t.Errorf("expected server to be set")
	}
	if client.token != "my-token" {
		t.Errorf("expected token to be set")
	}
}

func TestRegisterSingle(t *testing.T) {
	server, client := setupServer(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-token" {
			t.Errorf("expected X-API-Key header")
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/register" {
			t.Errorf("expected /register, got %s", r.URL.Path)
		}
		writeJSON(t, w, map[string]any{
			"id":         "abc123",
			"dns":        "abc123.hookd.example.com",
			"http":       "http://abc123.hookd.example.com",
			"https":      "https://abc123.hookd.example.com",
			"created_at": "2024-01-01T00:00:00Z",
		})
	})
	defer server.Close()

	hooks, err := client.Register(0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(hooks) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(hooks))
	}
	if hooks[0].ID != "abc123" {
		t.Errorf("expected id abc123, got %s", hooks[0].ID)
	}
	if hooks[0].DNS != "abc123.hookd.example.com" {
		t.Errorf("unexpected dns: %s", hooks[0].DNS)
	}
}

func TestRegisterMultiple(t *testing.T) {
	server, client := setupServer(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]int
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}
		if body["count"] != 3 {
			t.Errorf("expected count 3, got %d", body["count"])
		}
		writeJSON(t, w, map[string]any{
			"hooks": []any{
				map[string]any{"id": "h1", "dns": "h1.test", "http": "http://h1.test", "https": "https://h1.test", "created_at": "2024-01-01T00:00:00Z"},
				map[string]any{"id": "h2", "dns": "h2.test", "http": "http://h2.test", "https": "https://h2.test", "created_at": "2024-01-01T00:00:00Z"},
				map[string]any{"id": "h3", "dns": "h3.test", "http": "http://h3.test", "https": "https://h3.test", "created_at": "2024-01-01T00:00:00Z"},
			},
		})
	})
	defer server.Close()

	hooks, err := client.Register(3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(hooks) != 3 {
		t.Fatalf("expected 3 hooks, got %d", len(hooks))
	}
	if hooks[1].ID != "h2" {
		t.Errorf("expected h2, got %s", hooks[1].ID)
	}
}

func TestRegisterNegativeCount(t *testing.T) {
	client := NewClient("http://localhost", "token")
	_, err := client.Register(-1)
	if err == nil {
		t.Fatal("expected error for negative count")
	}
}

func TestPoll(t *testing.T) {
	server, client := setupServer(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/poll/abc123" {
			t.Errorf("expected /poll/abc123, got %s", r.URL.Path)
		}
		writeJSON(t, w, map[string]any{
			"interactions": []any{
				map[string]any{
					"type":      "dns",
					"timestamp": "2024-01-01T00:00:00Z",
					"source_ip": "1.2.3.4",
					"data":      map[string]any{"qname": "test.example.com", "qtype": "A"},
				},
				map[string]any{
					"type":      "http",
					"timestamp": "2024-01-01T00:01:00Z",
					"source_ip": "5.6.7.8",
					"data":      map[string]any{"method": "GET", "path": "/"},
				},
			},
		})
	})
	defer server.Close()

	interactions, err := client.Poll("abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(interactions) != 2 {
		t.Fatalf("expected 2 interactions, got %d", len(interactions))
	}
	if !interactions[0].IsDNS() {
		t.Error("expected first interaction to be DNS")
	}
	if !interactions[1].IsHTTP() {
		t.Error("expected second interaction to be HTTP")
	}
	if interactions[0].SourceIP != "1.2.3.4" {
		t.Errorf("unexpected source_ip: %s", interactions[0].SourceIP)
	}
}

func TestPollEmpty(t *testing.T) {
	server, client := setupServer(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{
			"interactions": []any{},
		})
	})
	defer server.Close()

	interactions, err := client.Poll("abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(interactions) != 0 {
		t.Errorf("expected 0 interactions, got %d", len(interactions))
	}
}

func TestPollBatch(t *testing.T) {
	server, client := setupServer(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/poll" {
			t.Errorf("expected /poll, got %s", r.URL.Path)
		}
		writeJSON(t, w, map[string]any{
			"results": map[string]any{
				"h1": map[string]any{
					"interactions": []any{
						map[string]any{"type": "dns", "timestamp": "2024-01-01T00:00:00Z", "source_ip": "1.2.3.4", "data": map[string]any{}},
					},
					"error": nil,
				},
				"h2": map[string]any{
					"interactions": []any{},
					"error":        nil,
				},
			},
		})
	})
	defer server.Close()

	results, err := client.PollBatch([]string{"h1", "h2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if len(results["h1"].Interactions) != 1 {
		t.Errorf("expected 1 interaction for h1, got %d", len(results["h1"].Interactions))
	}
	if results["h1"].Error != "" {
		t.Errorf("expected no error for h1")
	}
}

func TestPollBatchEmpty(t *testing.T) {
	client := NewClient("http://localhost", "token")
	_, err := client.PollBatch([]string{})
	if err == nil {
		t.Fatal("expected error for empty hook_ids")
	}
}

func TestMetrics(t *testing.T) {
	server, client := setupServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			t.Errorf("expected /metrics, got %s", r.URL.Path)
		}
		writeJSON(t, w, map[string]any{
			"total_hooks":        float64(42),
			"total_interactions": float64(100),
			"uptime_seconds":     float64(3600),
		})
	})
	defer server.Close()

	metrics, err := client.Metrics()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics["total_hooks"] != float64(42) {
		t.Errorf("unexpected total_hooks: %v", metrics["total_hooks"])
	}
}

func TestAuthenticationError(t *testing.T) {
	server, client := setupServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	})
	defer server.Close()

	_, err := client.Poll("abc123")
	if err == nil {
		t.Fatal("expected error")
	}
	var authErr *AuthenticationError
	if !errors.As(err, &authErr) {
		t.Errorf("expected AuthenticationError, got %T", err)
	}
}

func TestNotFoundError(t *testing.T) {
	server, client := setupServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})
	defer server.Close()

	_, err := client.Poll("nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
	var notFoundErr *NotFoundError
	if !errors.As(err, &notFoundErr) {
		t.Errorf("expected NotFoundError, got %T", err)
	}
}

func TestServerError(t *testing.T) {
	server, client := setupServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	})
	defer server.Close()

	_, err := client.Poll("abc123")
	if err == nil {
		t.Fatal("expected error")
	}
	var serverErr *ServerError
	if !errors.As(err, &serverErr) {
		t.Errorf("expected ServerError, got %T", err)
	}
}

func TestConnectionError(t *testing.T) {
	client := NewClient("http://localhost:1", "token")
	_, err := client.Poll("abc123")
	if err == nil {
		t.Fatal("expected error")
	}
	var connErr *ConnectionError
	if !errors.As(err, &connErr) {
		t.Errorf("expected ConnectionError, got %T", err)
	}
}

func TestInteractionHelpers(t *testing.T) {
	dns := Interaction{Type: "dns"}
	if !dns.IsDNS() {
		t.Error("expected IsDNS to be true")
	}
	if dns.IsHTTP() {
		t.Error("expected IsHTTP to be false")
	}

	h := Interaction{Type: "http"}
	if h.IsDNS() {
		t.Error("expected IsDNS to be false")
	}
	if !h.IsHTTP() {
		t.Error("expected IsHTTP to be true")
	}
}
