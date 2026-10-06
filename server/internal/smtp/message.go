package smtp

import (
	"mime"
	"net/mail"
	"strings"

	"github.com/jomar/hookd/internal/storage"
)

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

	return ss.receiveMessage()
}

// receiveMessage consumes DATA through its terminator before resetting the
// transaction, including when the message exceeds the configured limit.
func (ss *session) receiveMessage() bool {
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
