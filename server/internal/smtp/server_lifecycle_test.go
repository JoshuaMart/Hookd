package smtp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// acceptListener substitutes only Accept; these tests do not start a listener.
type acceptListener struct {
	net.Listener
	accept func() (net.Conn, error)
}

func (l acceptListener) Accept() (net.Conn, error) { return l.accept() }

func TestAcceptRetriesListenerErrors(t *testing.T) {
	serverConn, peer := net.Pipe()
	defer serverConn.Close()
	defer peer.Close()
	calls := 0
	srv := &Server{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		listener: acceptListener{accept: func() (net.Conn, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("temporary listener failure")
			}
			return serverConn, nil
		}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := srv.accept(ctx)
	if err != nil || conn != serverConn || calls != 2 {
		t.Fatalf("expected successful retry: conn=%v, err=%v, calls=%d", conn, err, calls)
	}
}

func TestAcceptStopsRetryingOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	srv := &Server{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		listener: acceptListener{accept: func() (net.Conn, error) {
			calls++
			cancel()
			return nil, net.ErrClosed
		}},
	}
	conn, err := srv.accept(ctx)
	if conn != nil || !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("expected cancellation without retry: conn=%v, err=%v, calls=%d", conn, err, calls)
	}
}
