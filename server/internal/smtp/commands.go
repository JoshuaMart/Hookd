package smtp

import (
	"fmt"
	"strings"
)

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

	id, tag, inDomain := hookIDFromRecipient(addr, ss.srv.domain, ss.srv.storage.Has)
	if !inDomain {
		return ss.reply("550 5.7.1 Relay access denied")
	}

	ss.rcpts = append(ss.rcpts, recipient{addr: addr, id: id, tag: tag})
	return ss.reply("250 2.1.5 Ok")
}
