package smtp

import (
	"time"
)

// Command-line cap from RFC 5321 4.5.3.1.4, which counts the trailing CRLF.
// Without it an unterminated line grows the buffer without bound.
const maxCommandLine = 512 - 2

// Longer lines are read in chunks, so this bounds memory, not line length.
const readBufferSize = 1024

// Independent of the read deadline: a timed-out session still has to send its
// 421, so it must not inherit a deadline that has already passed.
const writeTimeout = 30 * time.Second

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
