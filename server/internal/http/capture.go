package http

import (
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jomar/hookd/internal/netutil"
	"github.com/jomar/hookd/internal/storage"
)

// defaultMaxCaptureBodyBytes bounds a captured body when the config value is unset.
const defaultMaxCaptureBodyBytes = 1 << 20 // 1 MiB

// maxCaptureTargetBytes bounds the stored request target. The request line is
// already bounded by MaxHeaderBytes, but a 64 KiB target on every interaction
// is not worth keeping for weeks; 8 KiB matches what common servers accept.
const maxCaptureTargetBytes = 8192

// CaptureHandler handles wildcard HTTP requests
type CaptureHandler struct {
	storage      storage.Manager
	domain       string
	logger       *slog.Logger
	idGenerator  func() string
	maxBodyBytes int
}

// NewCaptureHandler creates a new capture handler
func NewCaptureHandler(storage storage.Manager, domain string, logger *slog.Logger, idGenerator func() string, maxBodyBytes int) *CaptureHandler {
	return &CaptureHandler{
		storage:      storage,
		domain:       domain,
		logger:       logger,
		idGenerator:  idGenerator,
		maxBodyBytes: maxBodyBytes,
	}
}

// ServeHTTP handles all wildcard HTTP requests
func (h *CaptureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Extract hook ID from Host header
	hookID := netutil.ResolveHookID(stripPort(r.Host), h.domain, h.storage.Has)

	if hookID == "" {
		// Not a valid hook subdomain
		w.WriteHeader(http.StatusOK)
		return
	}

	// Storage drops unknown hooks anyway; checking first skips the body read.
	if !h.storage.Has(hookID) {
		w.WriteHeader(http.StatusOK)
		return
	}

	limit := h.maxBodyBytes
	if limit <= 0 {
		limit = defaultMaxCaptureBodyBytes
	}

	// One byte past the cap tells a body that just fits from one that was cut.
	// Truncating rather than rejecting keeps the interaction, which is the signal.
	raw, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	if err != nil {
		h.logger.Error("failed to read request body", "error", err)
		raw = []byte{}
	}
	defer r.Body.Close()

	body, truncated := storage.TruncateBody(string(raw), limit)

	// Extract headers. A header sent several times carries a value per line;
	// joining keeps them all instead of silently dropping every line but the
	// first, which is where a payload may well sit.
	headers := make(map[string]string)
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = strings.Join(v, ", ")
		}
	}

	// The query string is half the request target and often carries the whole
	// point of the callback, so store the target rather than the bare path.
	target, targetTruncated := storage.TruncateBody(r.URL.RequestURI(), maxCaptureTargetBytes)

	// Create interaction
	sourceIP := netutil.ExtractIP(r.RemoteAddr)
	interaction := storage.HTTPInteraction(
		h.idGenerator(),
		sourceIP,
		r.Method,
		target,
		headers,
		body,
	)
	if truncated {
		interaction.Data["truncated"] = true
	}
	if targetTruncated {
		interaction.Data["path_truncated"] = true
	}

	// Store interaction
	h.storage.AddInteraction(hookID, interaction)

	h.logger.Debug("http interaction captured",
		"hook_id", hookID,
		"method", r.Method,
		"path", target,
		"client", sourceIP)

	// Respond with 200 OK
	w.WriteHeader(http.StatusOK)
}

// extractHookID extracts the hook ID from a host header.
// Example: abc123.hookd.jomar.ovh -> abc123
func (h *CaptureHandler) extractHookID(host string) string {
	return netutil.HookIDFromHost(stripPort(host), h.domain)
}

// stripPort removes a port from a Host header. An IPv6 literal is mangled by
// this, but such a host never matches the domain suffix anyway.
func stripPort(host string) string {
	if idx := strings.Index(host, ":"); idx != -1 {
		return host[:idx]
	}
	return host
}
