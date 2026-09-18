// Package smtp receives mail and records it as an interaction. Reception only:
// Hookd never originates mail, so there is no sending path here.
package smtp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/netutil"
	"github.com/jomar/hookd/internal/storage"
)

// Command-line cap from RFC 5321 4.5.3.1.4, which counts the trailing CRLF.
// Without it an unterminated line grows the buffer without bound.
const maxCommandLine = 512 - 2

// Longer lines are read in chunks, so this bounds memory, not line length.
const readBufferSize = 1024

// Independent of the read deadline: a timed-out session still has to send its
// 421, so it must not inherit a deadline that has already passed.
const writeTimeout = 30 * time.Second

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

// NewServer binds the listener here, so a port conflict or a missing
// CAP_NET_BIND_SERVICE surfaces at startup rather than from a goroutine.
// maxBodyBytes is the capture cap shared with the HTTP handler.
func NewServer(domain string, cfg config.SMTPConfig, maxBodyBytes int, store storage.Manager, logger *slog.Logger, idGenerator func() string) (*Server, error) {
	addr := net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("smtp listen on %s: %w", addr, err)
	}

	return &Server{
		domain:       strings.ToLower(strings.TrimSuffix(domain, ".")),
		cfg:          cfg,
		maxBodyBytes: maxBodyBytes,
		storage:      store,
		logger:       logger,
		idGenerator:  idGenerator,
		listener:     listener,
		sem:          make(chan struct{}, cfg.MaxConcurrent),
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

	var delay time.Duration
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.logger.Info("smtp server shutting down")
				return nil
			}
			// Never propagate: main cancels the root context on a Start error,
			// so an EMFILE here would take DNS and HTTP down with it.
			delay = nextAcceptDelay(delay)
			s.logger.Warn("smtp accept failed, retrying", "error", err, "retry_in", delay)
			select {
			case <-time.After(delay):
				continue
			case <-ctx.Done():
				s.logger.Info("smtp server shutting down")
				return nil
			}
		}
		delay = 0

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

// recipient is an accepted RCPT TO. The hook may not exist: unmatched
// recipients are dropped after DATA.
type recipient struct {
	addr string
	id   string
	tag  string
}

// session holds the state of one connection.
type session struct {
	srv      *Server
	conn     net.Conn
	br       *bufio.Reader
	bw       *bufio.Writer
	sourceIP string

	sessionEnd time.Time

	helo string

	// Distinguishes "no sender yet" from the legitimate null sender <>.
	inTransaction bool
	mailFrom      string
	rcpts         []recipient
}

// run drives the command loop until QUIT, an error, or a timeout.
func (ss *session) run() {
	if !ss.reply("220 " + ss.srv.domain + " ESMTP Hookd") {
		return
	}

	for {
		line, truncated, err := ss.readLine(maxCommandLine)
		if err != nil {
			ss.handleReadError(err)
			return
		}
		if truncated {
			if !ss.reply("500 5.5.1 Line too long") {
				return
			}
			continue
		}
		if !ss.command(line) {
			return
		}
	}
}

// handleReadError answers a timeout with 421; anything else is a closed pipe.
func (ss *session) handleReadError(err error) {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		ss.reply("421 4.4.2 Timeout, closing connection")
		return
	}
	ss.srv.logger.Debug("smtp session ended", "error", err, "client", ss.sourceIP)
}

// command dispatches one command. It returns false when the connection should
// be closed.
func (ss *session) command(line string) bool {
	verb, rest, _ := strings.Cut(line, " ")

	switch strings.ToUpper(verb) {
	case "EHLO":
		return ss.ehlo(rest, true)
	case "HELO":
		return ss.ehlo(rest, false)
	case "MAIL":
		return ss.mail(rest)
	case "RCPT":
		return ss.rcpt(rest)
	case "DATA":
		return ss.data()
	case "RSET":
		ss.resetTransaction()
		return ss.reply("250 2.0.0 Ok")
	case "NOOP":
		return ss.reply("250 2.0.0 Ok")
	case "QUIT":
		ss.reply("221 2.0.0 Bye")
		return false
	case "VRFY":
		// Never confirm or deny: hook IDs must not be enumerable.
		return ss.reply("252 2.5.2 Cannot VRFY user")
	default:
		return ss.reply("500 5.5.1 Command not recognized")
	}
}

// ehlo handles both greetings; HELO's reply carries no extension lines.
func (ss *session) ehlo(rest string, extended bool) bool {
	name := strings.TrimSpace(rest)
	if name == "" {
		return ss.reply("501 5.5.4 Syntax: EHLO hostname")
	}

	// A greeting resets any transaction in progress (RFC 5321 4.1.4).
	ss.helo = name
	ss.resetTransaction()

	if !extended {
		return ss.reply("250 " + ss.srv.domain)
	}
	return ss.reply(
		"250-"+ss.srv.domain,
		fmt.Sprintf("250-SIZE %d", ss.srv.cfg.MaxMessageBytes),
		"250 8BITMIME",
	)
}

// mail handles MAIL FROM, including the null sender and the SIZE parameter.
func (ss *session) mail(rest string) bool {
	arg, ok := cutPrefixFold(rest, "FROM:")
	if !ok {
		return ss.reply("501 5.5.4 Syntax: MAIL FROM:<address>")
	}

	if ss.inTransaction {
		return ss.reply("503 5.5.1 Sender already specified")
	}

	addr, params, ok := splitPath(arg)
	if !ok {
		return ss.reply("501 5.5.4 Syntax: MAIL FROM:<address>")
	}

	// Refusing here saves transferring a body DATA would reject anyway.
	if size, given := sizeParam(params); given && size > ss.srv.cfg.MaxMessageBytes {
		return ss.reply("552 5.3.4 Message size exceeds fixed limit")
	}

	ss.inTransaction = true
	ss.mailFrom = addr
	return ss.reply("250 2.1.0 Ok")
}

// rcpt accepts every address under the domain, hook or no hook. Refusing the
// ones outside it is what separates a capture server from an open relay.
func (ss *session) rcpt(rest string) bool {
	if !ss.inTransaction {
		return ss.reply("503 5.5.1 Need MAIL before RCPT")
	}
	arg, ok := cutPrefixFold(rest, "TO:")
	if !ok {
		return ss.reply("501 5.5.4 Syntax: RCPT TO:<address>")
	}

	addr, _, ok := splitPath(arg)
	if !ok || addr == "" {
		return ss.reply("501 5.5.4 Syntax: RCPT TO:<address>")
	}

	if len(ss.rcpts) >= ss.srv.cfg.MaxRecipients {
		return ss.reply("452 4.5.3 Too many recipients")
	}

	id, tag, inDomain := hookIDFromRecipient(addr, ss.srv.domain)
	if !inDomain {
		return ss.reply("550 5.7.1 Relay access denied")
	}

	ss.rcpts = append(ss.rcpts, recipient{addr: addr, id: id, tag: tag})
	return ss.reply("250 2.1.5 Ok")
}

// data reads the message and records it for every recipient with a live hook;
// the rest are dropped here, after the 250.
func (ss *session) data() bool {
	if !ss.inTransaction {
		return ss.reply("503 5.5.1 Need MAIL before DATA")
	}
	if len(ss.rcpts) == 0 {
		return ss.reply("503 5.5.1 Need RCPT before DATA")
	}
	if !ss.reply("354 End data with <CR><LF>.<CR><LF>") {
		return false
	}

	raw, truncated, tooBig, err := ss.readData()
	if err != nil {
		ss.handleReadError(err)
		return false
	}

	if tooBig {
		ss.resetTransaction()
		return ss.reply("552 5.3.4 Message size exceeds fixed limit")
	}

	rcptCount := len(ss.rcpts)
	captured := ss.deliver(raw, truncated)
	ss.resetTransaction()

	ss.srv.logger.Debug("smtp message received",
		"client", ss.sourceIP,
		"recipients", rcptCount,
		"captured", captured,
		"bytes", len(raw))

	return ss.reply("250 2.0.0 Ok")
}

// readData reads to the terminating lone dot. An oversized message is drained,
// not abandoned, so the connection stays in sync.
func (ss *session) readData() (raw string, truncated, tooBig bool, err error) {
	var (
		b    strings.Builder
		size int
	)

	// RFC 5321 caps a text line at 1000 octets, but real senders exceed it: an
	// 8bit HTML body is often one line, and a verification link is frequently
	// unwrapped. Cutting there would lose the very payload we capture, so the
	// line is bounded by the message cap, which already bounds memory.
	for {
		line, cut, err := ss.readLine(ss.srv.cfg.MaxMessageBytes)
		if err != nil {
			return "", false, false, err
		}
		if cut {
			truncated = true
		}

		if line == "." {
			return b.String(), truncated, tooBig, nil
		}

		// Dot-unstuffing (RFC 5321 4.5.2): a leading '.' arrives doubled.
		line = strings.TrimPrefix(line, ".")

		size += len(line) + 2 // the CRLF counts toward the advertised SIZE
		if size > ss.srv.cfg.MaxMessageBytes {
			tooBig = true
			continue
		}
		b.WriteString(line)
		b.WriteString("\r\n")
	}
}

// deliver records one interaction per matching recipient and returns the count.
func (ss *session) deliver(raw string, truncated bool) int {
	subject := parseSubject(raw)
	body, cut := storage.TruncateBody(raw, ss.srv.maxBodyBytes)
	truncated = truncated || cut

	captured := 0
	for _, r := range ss.rcpts {
		if !ss.srv.storage.Has(r.id) {
			continue
		}

		interaction := storage.SMTPInteraction(
			ss.srv.idGenerator(),
			ss.sourceIP,
			ss.helo,
			ss.mailFrom,
			r.addr,
			r.tag,
			subject,
			body,
		)
		if truncated {
			interaction.Data["truncated"] = true
		}

		ss.srv.storage.AddInteraction(r.id, interaction)
		captured++
	}
	return captured
}

// resetTransaction returns the session to the post-greeting state.
func (ss *session) resetTransaction() {
	ss.inTransaction = false
	ss.mailFrom = ""
	ss.rcpts = nil
}

// reply returns false when the write failed, which ends the session.
func (ss *session) reply(lines ...string) bool {
	if err := ss.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return false
	}
	for _, line := range lines {
		if _, err := ss.bw.WriteString(line + "\r\n"); err != nil {
			ss.srv.logger.Debug("smtp write failed", "error", err, "client", ss.sourceIP)
			return false
		}
	}
	if err := ss.bw.Flush(); err != nil {
		ss.srv.logger.Debug("smtp flush failed", "error", err, "client", ss.sourceIP)
		return false
	}
	return true
}

// readLine reads one line without its CRLF, capped at max bytes. An over-long
// line is drained so the next read starts at the following one.
func (ss *session) readLine(max int) (line string, truncated bool, err error) {
	if err := ss.conn.SetReadDeadline(ss.readDeadline()); err != nil {
		return "", false, err
	}

	var buf []byte
	for {
		chunk, isPrefix, err := ss.br.ReadLine()
		if err != nil {
			return "", false, err
		}

		if room := max - len(buf); room > 0 {
			if len(chunk) > room {
				buf = append(buf, chunk[:room]...)
				truncated = true
			} else {
				buf = append(buf, chunk...)
			}
		} else if len(chunk) > 0 {
			truncated = true
		}

		if !isPrefix {
			return string(buf), truncated, nil
		}
	}
}

// readDeadline is the earlier of the command timeout and the session end, so
// neither a slow command nor a long run of them holds the connection open.
func (ss *session) readDeadline() time.Time {
	d := time.Now().Add(ss.srv.cfg.ReadTimeout)
	if d.After(ss.sessionEnd) {
		return ss.sessionEnd
	}
	return d
}

// cutPrefixFold strips a case-insensitive prefix from the original string, so
// the address keeps its case and the offsets stay valid (ToUpper can resize).
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}

// splitPath returns the address and any trailing ESMTP parameters. The angle
// brackets are optional, since some clients omit them.
func splitPath(arg string) (addr, params string, ok bool) {
	arg = strings.TrimLeft(arg, " \t")

	if strings.HasPrefix(arg, "<") {
		end := strings.Index(arg, ">")
		if end < 0 {
			return "", "", false
		}
		return arg[1:end], strings.TrimSpace(arg[end+1:]), true
	}

	addr, params, _ = strings.Cut(arg, " ")
	if addr == "" {
		return "", "", false
	}
	return addr, strings.TrimSpace(params), true
}

// sizeParam reads SIZE=. When absent or unparseable the cap is enforced while
// reading DATA instead.
func sizeParam(params string) (size int, given bool) {
	for _, p := range strings.Fields(params) {
		value, ok := strings.CutPrefix(strings.ToUpper(p), "SIZE=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// parseSubject returns the decoded Subject. An unparseable message yields an
// empty subject rather than being dropped.
func parseSubject(raw string) string {
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		return ""
	}

	subject := msg.Header.Get("Subject")
	if subject == "" {
		return ""
	}

	// A non-ASCII subject arrives as RFC 2047 encoded-words.
	decoded, err := new(mime.WordDecoder).DecodeHeader(subject)
	if err != nil {
		return subject
	}
	return decoded
}
