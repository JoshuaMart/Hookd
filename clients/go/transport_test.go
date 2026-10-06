package hookd

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type responseTransport func(*http.Request) (*http.Response, error)

func (f responseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type observedBody struct {
	io.Reader
	reads  int
	closed bool
}

func (b *observedBody) Read(p []byte) (int, error) { b.reads++; return b.Reader.Read(p) }
func (b *observedBody) Close() error               { b.closed = true; return nil }

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestResponseBodyLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		reader    io.Reader
		wantReads bool
	}{
		{"status before read", 401, failedReader{}, false},
		{"read failure", 200, failedReader{}, true},
		{"invalid JSON", 200, strings.NewReader("invalid"), true},
		{"success", 200, strings.NewReader(`{}`), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &observedBody{Reader: tc.reader}
			client := NewClient("https://example.test", "token")
			client.httpClient.Transport = responseTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: body, Header: make(http.Header)}, nil
			})
			_, err := client.Metrics()
			switch tc.name {
			case "status before read":
				var target *AuthenticationError
				if !errors.As(err, &target) {
					t.Fatalf("expected authentication error, got %v", err)
				}
			case "read failure":
				var target *ConnectionError
				if !errors.As(err, &target) {
					t.Fatalf("expected connection error, got %v", err)
				}
			case "invalid JSON":
				var target *Error
				if !errors.As(err, &target) {
					t.Fatalf("expected JSON error, got %v", err)
				}
			case "success":
				if err != nil {
					t.Fatal(err)
				}
			}
			if !body.closed || (body.reads > 0) != tc.wantReads {
				t.Fatalf("unexpected body lifecycle: closed=%t, reads=%d", body.closed, body.reads)
			}
		})
	}
}

func TestResponseLimitBoundary(t *testing.T) {
	const response = `{"hooks":[]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, response)
	}))
	defer server.Close()
	for _, tc := range []struct {
		name     string
		limit    int64
		tooLarge bool
	}{
		{"exact", int64(len(response)), false},
		{"one byte over", int64(len(response) - 1), true},
		{"zero disables", 0, false},
		{"negative disables", -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := NewClient(server.URL, "token")
			client.SetMaxResponseBytes(tc.limit)
			_, err := client.Hooks(nil)
			var tooLarge *ResponseTooLargeError
			if tc.tooLarge {
				if !errors.As(err, &tooLarge) || tooLarge.Limit != tc.limit {
					t.Fatalf("expected size error, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
