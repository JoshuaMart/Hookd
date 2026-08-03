package http

import (
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthMiddleware(t *testing.T) {
	logger := slog.Default()
	token := "secret-token"

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("authenticated"))
	})

	middleware := AuthMiddleware(token, logger)

	t.Run("valid api key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-API-Key", "secret-token")
		w := httptest.NewRecorder()

		middleware(handler).ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		if w.Body.String() != "authenticated" {
			t.Errorf("expected body 'authenticated', got %s", w.Body.String())
		}
	})

	t.Run("missing api key header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()

		middleware(handler).ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected status 401, got %d", w.Code)
		}
	})

	t.Run("invalid api key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-API-Key", "wrong-token")
		w := httptest.NewRecorder()

		middleware(handler).ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected status 401, got %d", w.Code)
		}
	})
}

func TestLoggingMiddleware(t *testing.T) {
	logger := slog.Default()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	middleware := LoggingMiddleware(logger)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()

	middleware(handler).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
}

func TestRecoveryMiddleware(t *testing.T) {
	logger := slog.Default()

	panicHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})

	middleware := RecoveryMiddleware(logger)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	middleware(panicHandler).ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", w.Code)
	}
}

func TestRequireTLSMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("reached"))
	})

	t.Run("refuses plaintext when enforced", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/register", nil)
		req.Header.Set("X-API-Key", "secret-token")
		w := httptest.NewRecorder()

		RequireTLSMiddleware(true, slog.Default())(next).ServeHTTP(w, req)

		if w.Code != http.StatusUpgradeRequired {
			t.Errorf("expected status 426, got %d", w.Code)
		}
		if w.Body.String() == "reached" {
			t.Error("expected the request to be refused before the handler")
		}
		if got := w.Header().Get("Upgrade"); got == "" {
			t.Error("expected a 426 to advertise an Upgrade header")
		}
	})

	t.Run("allows TLS requests", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/register", nil)
		req.TLS = &tls.ConnectionState{}
		w := httptest.NewRecorder()

		RequireTLSMiddleware(true, slog.Default())(next).ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}
	})

	t.Run("allows plaintext when not enforced", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/register", nil)
		w := httptest.NewRecorder()

		RequireTLSMiddleware(false, slog.Default())(next).ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}
	})

	t.Run("warns only when a credential was exposed", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			apiKey    string
			wantWarns int
		}{
			{"with key", "secret-token", 1},
			{"without key", "", 0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := &recordingHandler{}
				req := httptest.NewRequest(http.MethodPost, "/register", nil)
				if tc.apiKey != "" {
					req.Header.Set("X-API-Key", tc.apiKey)
				}
				w := httptest.NewRecorder()

				RequireTLSMiddleware(true, slog.New(rec))(next).ServeHTTP(w, req)

				if rec.count != tc.wantWarns {
					t.Errorf("expected %d log records, got %d", tc.wantWarns, rec.count)
				}
			})
		}
	})

	t.Run("refuses before auth is consulted", func(t *testing.T) {
		authCalled := false
		auth := AuthMiddleware("secret-token", slog.Default())
		probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authCalled = true
			w.WriteHeader(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodPost, "/register", nil)
		req.Header.Set("X-API-Key", "secret-token")
		w := httptest.NewRecorder()

		RequireTLSMiddleware(true, slog.Default())(auth(probe)).ServeHTTP(w, req)

		if authCalled {
			t.Error("expected the token never to be examined on a plaintext request")
		}
		if w.Code != http.StatusUpgradeRequired {
			t.Errorf("expected status 426, got %d", w.Code)
		}
	})
}
