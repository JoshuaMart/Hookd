package http

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jomar/hookd/internal/netutil"
	"github.com/jomar/hookd/internal/storage"
)

// defaultMaxCaptureBodyBytes bounds a captured body when the config value is unset.
const defaultMaxCaptureBodyBytes = 1 << 20 // 1 MiB

// maxCaptureTargetBytes bounds the stored request target. The request line is
// already bounded by MaxHeaderBytes, but a 64 KiB target on every interaction
// is not worth keeping for weeks; 8 KiB matches what common servers accept.
const maxCaptureTargetBytes = 8192

// bodySlotWait is how long a capture waits for a body slot before recording
// the callback without its body. A burst gets through; a flood holding every
// slot still leaves a trace of each request.
const bodySlotWait = 5 * time.Second

// CaptureHandler handles wildcard HTTP requests
type CaptureHandler struct {
	storage      storage.Manager
	domain       string
	logger       *slog.Logger
	idGenerator  func() string
	maxBodyBytes int
	// bodySlots bounds the bodies being read at once; nil leaves them
	// unbounded. Each slot covers one body of up to maxBodyBytes.
	bodySlots chan struct{}
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

// SetBodySlots bounds how many request bodies are read at once. Each read
// buffers up to the body cap before the store's byte budget sees it, so
// without this, concurrent requests hold memory no budget accounts for. A
// non-positive n leaves reads unbounded. Call it before serving.
func (h *CaptureHandler) SetBodySlots(n int) {
	if n > 0 {
		h.bodySlots = make(chan struct{}, n)
	} else {
		h.bodySlots = nil
	}
}

// acquireBody reserves a body slot, waiting up to bodySlotWait. It reports
// whether the body may be read; release must be called when it returns true.
func (h *CaptureHandler) acquireBody(r *http.Request) bool {
	if h.bodySlots == nil {
		return true
	}
	select {
	case h.bodySlots <- struct{}{}:
		return true
	default:
	}
	timer := time.NewTimer(bodySlotWait)
	defer timer.Stop()
	select {
	case h.bodySlots <- struct{}{}:
		return true
	case <-timer.C:
		return false
	case <-r.Context().Done():
		return false
	}
}

func (h *CaptureHandler) releaseBody() {
	if h.bodySlots != nil {
		<-h.bodySlots
	}
}

// readBody reads the body up to limit, holding a body slot while it is
// buffered. skipped reports a body left unread because no slot freed up.
func (h *CaptureHandler) readBody(r *http.Request, limit int) (body string, truncated, skipped bool) {
	defer r.Body.Close()
	if r.ContentLength == 0 {
		return "", false, false
	}
	if !h.acquireBody(r) {
		return "", false, true
	}
	defer h.releaseBody()

	// One byte past the cap tells a body that just fits from one that was cut.
	// Truncating rather than rejecting keeps the interaction, which is the signal.
	raw, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	if err != nil {
		h.logger.Error("failed to read request body", "error", err)
		raw = []byte{}
	}
	body, truncated = storage.TruncateBody(string(raw), limit)
	return body, truncated, false
}

// ServeHTTP handles all wildcard HTTP requests
func (h *CaptureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Storage drops unknown hooks anyway; checking first skips the body read.
	hookID, found := netutil.ResolveHookID(stripPort(r.Host), h.domain, h.firstExisting)
	if !found {
		w.WriteHeader(http.StatusOK)
		return
	}

	limit := h.maxBodyBytes
	if limit <= 0 {
		limit = defaultMaxCaptureBodyBytes
	}

	body, truncated, skipped := h.readBody(r, limit)
	if skipped {
		// Debug: under a flood this fires per request; body_skipped marks it.
		h.logger.Debug("http body skipped: every body slot is busy", "hook_id", hookID)
	}

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
	if skipped {
		interaction.Data["body_skipped"] = true
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

func (h *CaptureHandler) firstExisting(ids ...string) int {
	return storage.FirstExisting(h.storage, ids...)
}
