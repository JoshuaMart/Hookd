package http

import (
	"log"
	"log/slog"
	"strings"
)

// suppressedTLSWriter wraps a logger to filter out TLS handshake errors
type suppressedTLSWriter struct {
	logger *slog.Logger
}

func (w *suppressedTLSWriter) Write(p []byte) (n int, err error) {
	msg := string(p)

	// Suppress TLS handshake errors (common from bots/scanners)
	if strings.Contains(msg, "TLS handshake error") ||
		strings.Contains(msg, "no certificate available") {
		return len(p), nil
	}

	// Log other errors through slog
	w.logger.Error("http server error", "message", strings.TrimSpace(msg))
	return len(p), nil
}

// newSuppressedTLSLogger creates a logger that suppresses TLS handshake errors
func newSuppressedTLSLogger(logger *slog.Logger) *log.Logger {
	return log.New(&suppressedTLSWriter{logger: logger}, "", 0)
}

// apiErrorWriter keeps TLS handshake errors visible on the private listener,
// where a failed handshake is a client misconfiguration, not scanner noise.
type apiErrorWriter struct {
	logger *slog.Logger
}

func (w *apiErrorWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimSpace(string(p))
	switch {
	case strings.Contains(msg, "no certificate available"):
		// certmagic found nothing for the client's SNI.
		w.logger.Warn("api listener tls handshake failed: clients must use server.domain as SNI", "message", msg)
	case strings.Contains(msg, "TLS handshake error"):
		w.logger.Debug("api listener tls handshake error", "message", msg)
	default:
		w.logger.Error("api server error", "message", msg)
	}
	return len(p), nil
}
