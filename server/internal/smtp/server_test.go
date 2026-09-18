package smtp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/storage"
)

const testDomain = "hookd.test.local"

func testConfig() config.SMTPConfig {
	cfg := config.DefaultConfig().Server.SMTP
	cfg.Enabled = true
	cfg.Port = 0 // let the kernel choose, so tests never collide
	cfg.BindAddress = "127.0.0.1"
	return cfg
}

// newTestServer starts a server on a loopback ephemeral port with an in-memory
// store, stopped when the test ends.
func newTestServer(t *testing.T, cfg config.SMTPConfig) (*Server, *storage.MemoryManager) {
	t.Helper()
	return newTestServerWithCap(t, cfg, 1<<20)
}

// newTestServerWithCap is newTestServer with an explicit capture-body cap.
func newTestServerWithCap(t *testing.T, cfg config.SMTPConfig, maxBodyBytes int) (*Server, *storage.MemoryManager) {
	t.Helper()

	idCounter := 0
	idGenerator := func() string {
		idCounter++
		return fmt.Sprintf("i%d", idCounter)
	}

	store := storage.NewMemoryManager(idGenerator)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := NewServer(testDomain, cfg, maxBodyBytes, store, logger, idGenerator)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Start(ctx); err != nil {
			t.Logf("smtp server stopped: %v", err)
		}
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return srv, store
}

// client is a minimal scripted SMTP client: it sends lines and reads replies.
type client struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

func dial(t *testing.T, srv *Server) *client {
	t.Helper()

	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	return &client{t: t, conn: conn, br: bufio.NewReader(conn)}
}

// send writes one command line.
func (c *client) send(format string, args ...any) {
	c.t.Helper()
	if _, err := fmt.Fprintf(c.conn, format+"\r\n", args...); err != nil {
		c.t.Fatalf("write %q: %v", format, err)
	}
}

// readReply reads one reply, following multi-line continuations.
func (c *client) readReply() string {
	c.t.Helper()

	var lines []string
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			c.t.Fatalf("read reply: %v (got %q so far)", err, strings.Join(lines, "|"))
		}
		line = strings.TrimRight(line, "\r\n")
		lines = append(lines, line)
		// A continuation line has a dash in the fourth column.
		if len(line) < 4 || line[3] != '-' {
			return strings.Join(lines, "\n")
		}
	}
}

// expect reads one reply and asserts its status code.
func (c *client) expect(code string) string {
	c.t.Helper()

	reply := c.readReply()
	last := reply
	if i := strings.LastIndex(reply, "\n"); i >= 0 {
		last = reply[i+1:]
	}
	if !strings.HasPrefix(last, code) {
		c.t.Fatalf("expected %s, got %q", code, reply)
	}
	return reply
}

// greet consumes the banner and completes an EHLO.
func (c *client) greet() {
	c.t.Helper()
	c.expect("220")
	c.send("EHLO sender.test")
	c.expect("250")
}

// deliver runs a whole nominal transaction.
func (c *client) deliver(from, to, message string) {
	c.t.Helper()
	c.send("MAIL FROM:<%s>", from)
	c.expect("250")
	c.send("RCPT TO:<%s>", to)
	c.expect("250")
	c.send("DATA")
	c.expect("354")
	if _, err := c.conn.Write([]byte(message + "\r\n.\r\n")); err != nil {
		c.t.Fatalf("write data: %v", err)
	}
}

func TestGreetingAndEHLO(t *testing.T) {
	srv, _ := newTestServer(t, testConfig())
	c := dial(t, srv)

	banner := c.expect("220")
	if !strings.Contains(banner, testDomain) || !strings.Contains(banner, "ESMTP") {
		t.Errorf("banner = %q, want it to name the domain and ESMTP", banner)
	}

	c.send("EHLO sender.test")
	reply := c.expect("250")
	if !strings.Contains(reply, "SIZE ") {
		t.Errorf("EHLO reply = %q, want a SIZE extension", reply)
	}
	if !strings.Contains(reply, "8BITMIME") {
		t.Errorf("EHLO reply = %q, want 8BITMIME", reply)
	}

	c.send("QUIT")
	c.expect("221")
}

func TestHELOHasNoExtensions(t *testing.T) {
	srv, _ := newTestServer(t, testConfig())
	c := dial(t, srv)
	c.expect("220")

	c.send("HELO sender.test")
	reply := c.expect("250")
	if strings.Contains(reply, "SIZE") || strings.Contains(reply, "\n") {
		t.Errorf("HELO reply = %q, want a single line with no extensions", reply)
	}
}

func TestNominalDelivery(t *testing.T) {
	srv, store := newTestServer(t, testConfig())
	hook := store.CreateHook(testDomain, storage.CreateOptions{SMTPEnabled: true})

	c := dial(t, srv)
	c.greet()
	c.deliver(
		"signup@vendor.test",
		hook.ID+"@"+testDomain,
		"From: signup@vendor.test\r\nSubject: Verify your account\r\n\r\ncode 1234",
	)
	c.expect("250")

	interactions := store.PollInteractions(hook.ID)
	if len(interactions) != 1 {
		t.Fatalf("got %d interactions, want 1", len(interactions))
	}

	got := interactions[0]
	if got.Type != storage.InteractionTypeSMTP {
		t.Errorf("type = %q, want %q", got.Type, storage.InteractionTypeSMTP)
	}
	if got.SourceIP != "127.0.0.1" {
		t.Errorf("source_ip = %q, want 127.0.0.1", got.SourceIP)
	}
	assertData(t, got, "helo", "sender.test")
	assertData(t, got, "mail_from", "signup@vendor.test")
	assertData(t, got, "rcpt_to", hook.ID+"@"+testDomain)
	assertData(t, got, "tag", "")
	assertData(t, got, "subject", "Verify your account")

	body, _ := got.Data["body"].(string)
	if !strings.Contains(body, "code 1234") {
		t.Errorf("body = %q, want it to contain the message text", body)
	}
	if !strings.Contains(body, "Subject: Verify your account") {
		t.Errorf("body = %q, want the raw message including its headers", body)
	}
	if _, flagged := got.Data["truncated"]; flagged {
		t.Error("a message well under the cap should not be flagged as truncated")
	}
}

func TestSubAddressTagIsRecorded(t *testing.T) {
	srv, store := newTestServer(t, testConfig())
	hook := store.CreateHook(testDomain, storage.CreateOptions{SMTPEnabled: true})

	c := dial(t, srv)
	c.greet()
	c.deliver("x@vendor.test", hook.ID+"+carrefour@"+testDomain, "Subject: hi\r\n\r\nbody")
	c.expect("250")

	interactions := store.PollInteractions(hook.ID)
	if len(interactions) != 1 {
		t.Fatalf("got %d interactions, want 1", len(interactions))
	}
	assertData(t, interactions[0], "tag", "carrefour")
}

func TestSubdomainFallbackRoutes(t *testing.T) {
	srv, store := newTestServer(t, testConfig())
	hook := store.CreateHook(testDomain, storage.CreateOptions{SMTPEnabled: true})

	c := dial(t, srv)
	c.greet()
	c.deliver("x@vendor.test", "anything@"+hook.ID+"."+testDomain, "Subject: hi\r\n\r\nbody")
	c.expect("250")

	if n := len(store.PollInteractions(hook.ID)); n != 1 {
		t.Fatalf("got %d interactions, want 1", n)
	}
}

func TestRelayIsRefused(t *testing.T) {
	srv, _ := newTestServer(t, testConfig())

	c := dial(t, srv)
	c.greet()
	c.send("MAIL FROM:<x@vendor.test>")
	c.expect("250")
	c.send("RCPT TO:<victim@example.org>")
	c.expect("550")
}

// Accepting then dropping is what keeps hook IDs unenumerable.
func TestUnknownHookIsAcceptedThenDropped(t *testing.T) {
	srv, store := newTestServer(t, testConfig())

	c := dial(t, srv)
	c.greet()
	c.deliver("x@vendor.test", "nosuchhook@"+testDomain, "Subject: hi\r\n\r\nbody")
	c.expect("250")

	if stats := store.Stats(); stats.InteractionsTotal != 0 {
		t.Errorf("stored %d interactions for an unknown hook, want 0", stats.InteractionsTotal)
	}
}

// A form that probes the address without sending must see a clean 250.
func TestProbeWithoutDataSucceeds(t *testing.T) {
	srv, _ := newTestServer(t, testConfig())

	c := dial(t, srv)
	c.greet()
	c.send("MAIL FROM:<probe@vendor.test>")
	c.expect("250")
	c.send("RCPT TO:<nosuchhook@%s>", testDomain)
	c.expect("250")
	c.send("QUIT")
	c.expect("221")
}

func TestDotStuffedBodyRoundTrip(t *testing.T) {
	srv, store := newTestServer(t, testConfig())
	hook := store.CreateHook(testDomain, storage.CreateOptions{SMTPEnabled: true})

	c := dial(t, srv)
	c.greet()
	// ".." on the wire is one "."; without unstuffing both lines corrupt.
	c.deliver("x@vendor.test", hook.ID+"@"+testDomain,
		"Subject: dots\r\n\r\nbefore\r\n..\r\n..leading dot\r\nafter")
	c.expect("250")

	interactions := store.PollInteractions(hook.ID)
	if len(interactions) != 1 {
		t.Fatalf("got %d interactions, want 1", len(interactions))
	}

	body, _ := interactions[0].Data["body"].(string)
	want := "Subject: dots\r\n\r\nbefore\r\n.\r\n.leading dot\r\nafter\r\n"
	if body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

func TestOversizedMessageIsRejected(t *testing.T) {
	cfg := testConfig()
	cfg.MaxMessageBytes = 512
	srv, store := newTestServer(t, cfg)
	hook := store.CreateHook(testDomain, storage.CreateOptions{SMTPEnabled: true})

	c := dial(t, srv)
	c.greet()
	c.deliver("x@vendor.test", hook.ID+"@"+testDomain,
		"Subject: big\r\n\r\n"+strings.Repeat("x", 2048))
	c.expect("552")

	if n := len(store.PollInteractions(hook.ID)); n != 0 {
		t.Errorf("stored %d interactions for an oversized message, want 0", n)
	}

	// Still usable: the oversized message was drained, not abandoned.
	c.send("NOOP")
	c.expect("250")
}

func TestOversizedSizeParameterIsRefusedEarly(t *testing.T) {
	cfg := testConfig()
	cfg.MaxMessageBytes = 512
	srv, _ := newTestServer(t, cfg)

	c := dial(t, srv)
	c.greet()
	c.send("MAIL FROM:<x@vendor.test> SIZE=99999")
	c.expect("552")
}

func TestTooManyRecipients(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRecipients = 2
	srv, _ := newTestServer(t, cfg)

	c := dial(t, srv)
	c.greet()
	c.send("MAIL FROM:<x@vendor.test>")
	c.expect("250")
	for i := 0; i < cfg.MaxRecipients; i++ {
		c.send("RCPT TO:<r%d@%s>", i, testDomain)
		c.expect("250")
	}
	c.send("RCPT TO:<overflow@%s>", testDomain)
	c.expect("452")
}

// One message addressed to three hooks yields three interactions.
func TestOneInteractionPerMatchingRecipient(t *testing.T) {
	srv, store := newTestServer(t, testConfig())

	hooks := make([]string, 3)
	for i := range hooks {
		hooks[i] = store.CreateHook(testDomain, storage.CreateOptions{SMTPEnabled: true}).ID
	}

	c := dial(t, srv)
	c.greet()
	c.send("MAIL FROM:<x@vendor.test>")
	c.expect("250")
	for _, id := range hooks {
		c.send("RCPT TO:<%s@%s>", id, testDomain)
		c.expect("250")
	}
	// A recipient that matches nothing must not add a fourth.
	c.send("RCPT TO:<nosuchhook@%s>", testDomain)
	c.expect("250")

	c.send("DATA")
	c.expect("354")
	if _, err := c.conn.Write([]byte("Subject: fan-out\r\n\r\nbody\r\n.\r\n")); err != nil {
		t.Fatalf("write data: %v", err)
	}
	c.expect("250")

	for _, id := range hooks {
		if n := len(store.PollInteractions(id)); n != 1 {
			t.Errorf("hook %s got %d interactions, want 1", id, n)
		}
	}
}

func TestOutOfOrderCommands(t *testing.T) {
	srv, _ := newTestServer(t, testConfig())

	c := dial(t, srv)
	c.greet()

	c.send("RCPT TO:<x@%s>", testDomain)
	c.expect("503")

	c.send("DATA")
	c.expect("503")

	c.send("MAIL FROM:<x@vendor.test>")
	c.expect("250")

	c.send("MAIL FROM:<y@vendor.test>")
	c.expect("503")

	c.send("DATA")
	c.expect("503") // sender set, but no recipient
}

func TestRSETClearsTheTransaction(t *testing.T) {
	srv, _ := newTestServer(t, testConfig())

	c := dial(t, srv)
	c.greet()
	c.send("MAIL FROM:<x@vendor.test>")
	c.expect("250")
	c.send("RCPT TO:<x@%s>", testDomain)
	c.expect("250")

	c.send("RSET")
	c.expect("250")

	// Back to the post-greeting state: RCPT is premature again.
	c.send("RCPT TO:<x@%s>", testDomain)
	c.expect("503")
}

func TestUnknownCommandAndVRFY(t *testing.T) {
	srv, _ := newTestServer(t, testConfig())

	c := dial(t, srv)
	c.greet()

	c.send("STARTTLS")
	c.expect("500")

	// VRFY never confirms or denies an address.
	c.send("VRFY postmaster")
	c.expect("252")
}

func TestPipelinedCommands(t *testing.T) {
	srv, store := newTestServer(t, testConfig())
	hook := store.CreateHook(testDomain, storage.CreateOptions{SMTPEnabled: true})

	c := dial(t, srv)
	c.expect("220")

	// Everything up to DATA in one write, as a pipelining client sends it.
	batch := fmt.Sprintf("EHLO sender.test\r\nMAIL FROM:<x@vendor.test>\r\nRCPT TO:<%s@%s>\r\nDATA\r\n",
		hook.ID, testDomain)
	if _, err := c.conn.Write([]byte(batch)); err != nil {
		t.Fatalf("write batch: %v", err)
	}

	c.expect("250") // EHLO
	c.expect("250") // MAIL
	c.expect("250") // RCPT
	c.expect("354") // DATA

	if _, err := c.conn.Write([]byte("Subject: pipelined\r\n\r\nbody\r\n.\r\n")); err != nil {
		t.Fatalf("write data: %v", err)
	}
	c.expect("250")

	if n := len(store.PollInteractions(hook.ID)); n != 1 {
		t.Errorf("got %d interactions, want 1", n)
	}
}

func TestOverlongCommandLineIsRefused(t *testing.T) {
	srv, _ := newTestServer(t, testConfig())

	c := dial(t, srv)
	c.greet()

	c.send("NOOP %s", strings.Repeat("x", 4096))
	c.expect("500")

	// The rest of the over-long line was drained, so the session is still in
	// sync with the client.
	c.send("NOOP")
	c.expect("250")
}

func TestIdleSessionTimesOut(t *testing.T) {
	cfg := testConfig()
	cfg.ReadTimeout = 150 * time.Millisecond
	cfg.SessionTimeout = time.Second
	srv, _ := newTestServer(t, cfg)

	c := dial(t, srv)
	c.expect("220")

	// Say nothing; the per-command deadline must close the session.
	c.expect("421")
}

func TestSessionTimeoutBoundsABusySession(t *testing.T) {
	cfg := testConfig()
	cfg.ReadTimeout = time.Minute
	cfg.SessionTimeout = 300 * time.Millisecond
	srv, _ := newTestServer(t, cfg)

	c := dial(t, srv)
	c.expect("220")

	// A client that keeps talking still cannot outlast the session deadline.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.send("NOOP")
		reply := c.readReply()
		if strings.HasPrefix(reply, "421") {
			return
		}
		if !strings.HasPrefix(reply, "250") {
			t.Fatalf("unexpected reply %q", reply)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("session was never closed by the session timeout")
}

func TestConcurrencyCap(t *testing.T) {
	cfg := testConfig()
	cfg.MaxConcurrent = 1
	srv, _ := newTestServer(t, cfg)

	// Hold the single slot.
	first := dial(t, srv)
	first.expect("220")

	// The second connection is answered 421 and closed.
	second := dial(t, srv)
	second.expect("421")
	if _, err := second.br.ReadString('\n'); err == nil {
		t.Error("expected the refused connection to be closed")
	}
}

// A long-lived hook truncates the stored message at its own cap, the same way
// it truncates an HTTP body.
func TestBodyTruncatedAtCaptureCap(t *testing.T) {
	srv, store := newTestServerWithCap(t, testConfig(), 64)

	hook := store.CreateHook(testDomain, storage.CreateOptions{SMTPEnabled: true})

	c := dial(t, srv)
	c.greet()
	c.deliver("x@vendor.test", hook.ID+"@"+testDomain,
		"Subject: long\r\n\r\n"+strings.Repeat("y", 500))
	c.expect("250")

	interactions := store.PollInteractions(hook.ID)
	if len(interactions) != 1 {
		t.Fatalf("got %d interactions, want 1", len(interactions))
	}

	body, _ := interactions[0].Data["body"].(string)
	if len(body) > 64 {
		t.Errorf("body is %d bytes, want it capped at 64", len(body))
	}
	if truncated, _ := interactions[0].Data["truncated"].(bool); !truncated {
		t.Error("expected the interaction to be flagged as truncated")
	}
}

func TestNullSenderIsAccepted(t *testing.T) {
	srv, store := newTestServer(t, testConfig())
	hook := store.CreateHook(testDomain, storage.CreateOptions{SMTPEnabled: true})

	c := dial(t, srv)
	c.greet()
	// A bounce arrives with the null sender, and is itself a signal.
	c.deliver("", hook.ID+"@"+testDomain, "Subject: returned mail\r\n\r\nbody")
	c.expect("250")

	interactions := store.PollInteractions(hook.ID)
	if len(interactions) != 1 {
		t.Fatalf("got %d interactions, want 1", len(interactions))
	}
	assertData(t, interactions[0], "mail_from", "")
}

func assertData(t *testing.T, interaction *storage.Interaction, key, want string) {
	t.Helper()
	got, _ := interaction.Data[key].(string)
	if got != want {
		t.Errorf("data[%q] = %q, want %q", key, got, want)
	}
}

// A long unwrapped line — an 8bit HTML body, or a verification link — must
// survive intact up to the message cap, not be cut at the RFC's 1000 octets.
func TestLongBodyLineSurvivesIntact(t *testing.T) {
	srv, store := newTestServer(t, testConfig())
	hook := store.CreateHook(testDomain, storage.CreateOptions{SMTPEnabled: true})

	link := "https://vendor.test/verify?token=" + strings.Repeat("a", 4000)

	c := dial(t, srv)
	c.greet()
	c.deliver("x@vendor.test", hook.ID+"@"+testDomain, "Subject: verify\r\n\r\n"+link)
	c.expect("250")

	interactions := store.PollInteractions(hook.ID)
	if len(interactions) != 1 {
		t.Fatalf("got %d interactions, want 1", len(interactions))
	}

	body, _ := interactions[0].Data["body"].(string)
	if !strings.Contains(body, link) {
		t.Errorf("the link was cut: body is %d bytes for a %d-byte line", len(body), len(link))
	}
	if _, flagged := interactions[0].Data["truncated"]; flagged {
		t.Error("a line under the message cap should not flag the message as truncated")
	}
}
