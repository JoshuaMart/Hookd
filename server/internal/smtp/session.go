package smtp

import (
	"bufio"
	"errors"
	"net"
	"time"
)

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

// resetTransaction returns the session to the post-greeting state.
func (ss *session) resetTransaction() {
	ss.inTransaction = false
	ss.mailFrom = ""
	ss.rcpts = nil
}
