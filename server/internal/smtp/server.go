// Package smtp receives mail and records it as an interaction. Reception only:
// Hookd never originates mail, so there is no sending path here.
package smtp

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/netutil"
	"github.com/jomar/hookd/internal/storage"
)

// refuseWriteTimeout bounds the 421 sent to a connection over max_concurrent.
// It is short because a peer that will not read a 46-byte greeting is hostile.
const refuseWriteTimeout = 2 * time.Second

// Accept backoff bounds, following net/http.Server.Serve.
const (
	minAcceptDelay = 5 * time.Millisecond
	maxAcceptDelay = time.Second
)

// Server accepts mail for every address under the domain and records the ones
// that map to a live hook.
type Server struct {
	domain       string
	cfg          config.SMTPConfig
	maxBodyBytes int
	storage      storage.Manager
	logger       *slog.Logger
	idGenerator  func() string

	listener net.Listener
	sem      chan struct{}

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// ServerOptions groups SMTP configuration and runtime dependencies.
type ServerOptions struct {
	Domain       string
	SMTP         config.SMTPConfig
	MaxBodyBytes int
	Storage      storage.Manager
	Logger       *slog.Logger
	IDGenerator  func() string
}

// NewServer binds the listener here, so a port conflict or a missing
// CAP_NET_BIND_SERVICE surfaces at startup rather than from a goroutine.
// MaxBodyBytes is the capture cap shared with the HTTP handler.
func NewServer(opts ServerOptions) (*Server, error) {
	addr := net.JoinHostPort(opts.SMTP.BindAddress, strconv.Itoa(opts.SMTP.Port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("smtp listen on %s: %w", addr, err)
	}
	return &Server{
		domain:       strings.ToLower(strings.TrimSuffix(opts.Domain, ".")),
		cfg:          opts.SMTP,
		maxBodyBytes: opts.MaxBodyBytes,
		storage:      opts.Storage,
		logger:       opts.Logger,
		idGenerator:  opts.IDGenerator,
		listener:     listener,
		sem:          make(chan struct{}, opts.SMTP.MaxConcurrent),
		conns:        make(map[net.Conn]struct{}),
	}, nil
}

// Addr is how a caller that asked for port 0 finds the port it was given.
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

// Start accepts connections until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	s.logger.Info("smtp server starting",
		"listen_addr", s.Addr(),
		"domain", s.domain,
		"max_message_bytes", s.cfg.MaxMessageBytes,
		"max_concurrent", s.cfg.MaxConcurrent)

	go func() {
		<-ctx.Done()
		_ = s.listener.Close()
		s.closeAll()
	}()

	for {
		conn, err := s.accept(ctx)
		if err != nil {
			s.logger.Info("smtp server shutting down")
			return nil
		}
		s.dispatch(conn)
	}
}

// accept retries listener errors until cancellation. Propagating an accept
// error would cancel the root context and stop DNS and HTTP as well.
func (s *Server) accept(ctx context.Context) (net.Conn, error) {
	var delay time.Duration
	for {
		conn, err := s.listener.Accept()
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		delay = nextAcceptDelay(delay)
		s.logger.Warn("smtp accept failed, retrying", "error", err, "retry_in", delay)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// dispatch reserves a session slot or refuses the connection without blocking
// the listener on a session's reads and writes.
func (s *Server) dispatch(conn net.Conn) {
	select {
	case s.sem <- struct{}{}:
		go func() {
			defer func() { <-s.sem }()
			s.serve(conn)
		}()
	default:
		s.refuse(conn)
	}
}

// refuse answers 421 rather than dropping the socket, so a legitimate sender
// knows to retry. The write runs off the accept loop: a peer that never reads
// would otherwise stall every further connection for the write deadline.
func (s *Server) refuse(conn net.Conn) {
	// RemoteAddr is only defined while the connection is open.
	client := netutil.ExtractIP(conn.RemoteAddr().String())
	s.logger.Warn("smtp connection refused", "reason", "max_concurrent", "client", client)

	go func() {
		_ = conn.SetWriteDeadline(time.Now().Add(refuseWriteTimeout))
		_, _ = conn.Write([]byte("421 4.7.0 Too many connections, try again later\r\n"))
		_ = conn.Close()
	}()
}

// nextAcceptDelay doubles the retry delay between the accept backoff bounds.
func nextAcceptDelay(d time.Duration) time.Duration {
	if d == 0 {
		return minAcceptDelay
	}
	if d *= 2; d > maxAcceptDelay {
		return maxAcceptDelay
	}
	return d
}

// serve runs one session to completion.
func (s *Server) serve(conn net.Conn) {
	s.track(conn)
	defer func() {
		s.untrack(conn)
		_ = conn.Close()
		// One bad session must not take the listener down with it.
		if r := recover(); r != nil {
			s.logger.Error("smtp session panic", "error", r)
		}
	}()

	(&session{
		srv:        s,
		conn:       conn,
		br:         bufio.NewReaderSize(conn, readBufferSize),
		bw:         bufio.NewWriter(conn),
		sourceIP:   netutil.ExtractIP(conn.RemoteAddr().String()),
		sessionEnd: time.Now().Add(s.cfg.SessionTimeout),
	}).run()
}

func (s *Server) track(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conns[conn] = struct{}{}
}

func (s *Server) untrack(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, conn)
}

// closeAll drops live sessions so shutdown is not delayed by session_timeout.
func (s *Server) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for conn := range s.conns {
		_ = conn.Close()
	}
}
