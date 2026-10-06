package http

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jomar/hookd/internal/config"
)

func TestStartHTTPReportsBindFailure(t *testing.T) {
	// Match startHTTP's wildcard bind: on macOS, a loopback-only bind can
	// coexist with a wildcard listener on the other IP family.
	occupied, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	s := &Server{
		config: config.ServerConfig{HTTP: config.HTTPConfig{Port: occupied.Addr().(*net.TCPAddr).Port}},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	errorsCh := make(chan error, 1)
	s.startHTTP(http.NewServeMux(), errorsCh)
	defer s.httpServer.Close()
	select {
	case err := <-errorsCh:
		var opErr *net.OpError
		if !errors.As(err, &opErr) || !strings.Contains(err.Error(), "http server error") {
			t.Fatalf("expected wrapped listener error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listener error was not reported")
	}
}

func TestShutdownClosesBothListeners(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{
		logger:      logger,
		httpServer:  newPublicServer("", http.NewServeMux(), logger),
		httpsServer: newPublicServer("", http.NewServeMux(), logger),
	}
	done := make(chan error, 2)
	for _, server := range []*http.Server{s.httpServer, s.httpsServer} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close(); server.Close() })
		go func() { done <- server.Serve(listener) }()
	}
	s.shutdown()
	for range 2 {
		select {
		case err := <-done:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Fatalf("expected closed server, got %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("listener did not stop")
		}
	}
}
