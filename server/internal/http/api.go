package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/eviction"
	"github.com/jomar/hookd/internal/netutil"
	"github.com/jomar/hookd/internal/storage"
)

// defaultMaxMetadataBytes bounds hook metadata when the config value is unset.
const defaultMaxMetadataBytes = 8192

// APIHandler handles API endpoints
type APIHandler struct {
	storage     storage.Manager
	evictor     *eviction.Evictor
	domain      string
	longLived   config.LongLivedConfig
	logger      *slog.Logger
	idGenerator func() string
}

// NewAPIHandler creates a new API handler
func NewAPIHandler(storage storage.Manager, evictor *eviction.Evictor, domain string, longLived config.LongLivedConfig, logger *slog.Logger, idGenerator func() string) *APIHandler {
	return &APIHandler{
		storage:     storage,
		evictor:     evictor,
		domain:      domain,
		longLived:   longLived,
		logger:      logger,
		idGenerator: idGenerator,
	}
}

// HandleRegister handles POST /register
func (h *APIHandler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "Method not allowed",
		})
		return
	}

	// Only parse the body if it exists. A malformed body falls back to a single
	// ephemeral hook, preserving the historical lenient behavior.
	var req registerRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			req = registerRequest{}
		}
	}

	// Default to 1 if count not specified or invalid
	if req.Count < 1 {
		req.Count = 1
	}

	const maxHooks = 100
	if req.Count > maxHooks {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "count must not exceed 100"})
		return
	}

	opts, errResp := h.buildCreateOptions(req.TTL, req.Metadata)
	if errResp != nil {
		respondJSON(w, errResp.status, map[string]string{"error": errResp.message})
		return
	}

	longLived := opts.TTL > h.evictor.HookTTL()

	// Reject an over-cap batch up front so no hooks are created when the request
	// cannot be satisfied in full. Each create is still atomically capped below,
	// which is what enforces the bound under concurrency.
	if longLived {
		if llm, ok := h.storage.(storage.LongLivedManager); ok {
			if llm.LongLivedCount()+req.Count > h.longLived.MaxHooks {
				respondJSON(w, http.StatusTooManyRequests, map[string]string{
					"error": "long-lived hook limit reached",
				})
				return
			}
		}
	}

	hooks := make([]interface{}, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		hook, errResp := h.createHook(opts, longLived)
		if errResp != nil {
			respondJSON(w, errResp.status, map[string]string{"error": errResp.message})
			return
		}
		hooks = append(hooks, hook)
	}

	h.logger.Info("hooks created", "count", req.Count, "long_lived", longLived, "client", r.RemoteAddr)

	// Single-hook registrations return the hook object directly for backward
	// compatibility; batches return a hooks array.
	if req.Count == 1 {
		respondJSON(w, http.StatusOK, hooks[0])
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"hooks": hooks})
}

// registerRequest is the optional body of POST /register.
type registerRequest struct {
	Count    int            `json:"count,omitempty"`
	TTL      string         `json:"ttl,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// createHook creates one hook, routing long-lived registrations through the
// fallible, cap-enforcing path so a full store or a persistence failure surfaces
// as a proper HTTP error instead of a hook that silently never captures.
func (h *APIHandler) createHook(opts storage.CreateOptions, longLived bool) (*storage.Hook, *apiError) {
	if !longLived {
		return h.storage.CreateHook(h.domain, opts), nil
	}

	llm, ok := h.storage.(storage.LongLivedManager)
	if !ok {
		return nil, &apiError{http.StatusBadRequest, "long-lived hooks are disabled"}
	}
	hook, err := llm.CreateLongLivedHook(h.domain, opts, h.longLived.MaxHooks)
	if errors.Is(err, storage.ErrHookLimitReached) {
		return nil, &apiError{http.StatusTooManyRequests, "long-lived hook limit reached"}
	}
	if err != nil {
		h.logger.Error("failed to create long-lived hook", "error", err)
		return nil, &apiError{http.StatusInternalServerError, "failed to create hook"}
	}
	return hook, nil
}

// apiError bundles an HTTP status with a client-facing message.
type apiError struct {
	status  int
	message string
}

// buildCreateOptions validates the optional ttl and metadata and returns the
// resolved storage options. A nil apiError means success.
//
// TTL semantics: absent means ephemeral (the configured hook TTL). A value at or
// below the ephemeral hook TTL is rejected — ephemeral hooks are requested by
// omitting ttl. A value above it designates a long-lived hook, capped at
// long_lived.max_ttl, and requires the long-lived store to be enabled.
func (h *APIHandler) buildCreateOptions(ttl string, metadata map[string]any) (storage.CreateOptions, *apiError) {
	opts := storage.CreateOptions{Metadata: metadata}

	// Metadata is stored for ephemeral hooks too, so the size cap is enforced
	// unconditionally (not gated on the long-lived feature). Fall back to a
	// sane default if the configured value is unset.
	if metadata != nil {
		limit := h.longLived.MaxMetadataBytes
		if limit <= 0 {
			limit = defaultMaxMetadataBytes
		}
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return opts, &apiError{http.StatusBadRequest, "invalid metadata"}
		}
		if len(encoded) > limit {
			return opts, &apiError{http.StatusBadRequest, fmt.Sprintf("metadata must not exceed %d bytes", limit)}
		}
	}

	ephemeralTTL := h.evictor.HookTTL()
	if ttl == "" {
		opts.TTL = ephemeralTTL
		return opts, nil
	}

	d, err := parseTTL(ttl)
	if err != nil {
		return opts, &apiError{http.StatusBadRequest, "invalid ttl (use a Go duration like \"168h\" or a day count like \"7d\")"}
	}
	if d <= ephemeralTTL {
		return opts, &apiError{http.StatusBadRequest, fmt.Sprintf("ttl must exceed the ephemeral hook ttl (%s); omit ttl for ephemeral hooks", ephemeralTTL)}
	}
	if !h.longLived.Enabled {
		return opts, &apiError{http.StatusBadRequest, "long-lived hooks are disabled"}
	}
	if d > h.longLived.MaxTTL {
		return opts, &apiError{http.StatusBadRequest, fmt.Sprintf("ttl must not exceed %s", h.longLived.MaxTTL)}
	}
	opts.TTL = d
	return opts, nil
}

// maxTTLDays bounds the day-count form of ttl so the nanosecond conversion
// cannot overflow int64 (which would silently wrap and bypass the max_ttl cap).
// ~106751 days is the int64-nanosecond ceiling; 100000 leaves margin and is far
// beyond any real hook lifetime.
const maxTTLDays = 100000

// parseTTL accepts a Go duration ("168h", "90m") or a plain day count ("7d").
func parseTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("invalid day count: %q", s)
		}
		if n < 0 || n > maxTTLDays {
			return 0, fmt.Errorf("day count out of range: %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// HandlePollBatch handles POST /poll (batch polling)
func (h *APIHandler) HandlePollBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "Method not allowed",
		})
		return
	}

	// Parse request body as array of hook IDs
	var hookIDs []string

	if err := json.NewDecoder(r.Body).Decode(&hookIDs); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Invalid request body",
		})
		return
	}

	// Validate request
	if len(hookIDs) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error": "hook_ids cannot be empty",
		})
		return
	}

	// Poll interactions for all hooks
	results := h.storage.PollInteractionsBatch(hookIDs)

	h.logger.Info("batch interactions polled",
		"hook_count", len(hookIDs),
		"client", r.RemoteAddr)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"results": results,
	})
}

// HandlePoll handles GET /poll/:id
func (h *APIHandler) HandlePoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "Method not allowed",
		})
		return
	}

	// Extract hook ID from path
	// Path format: /poll/abc123
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 2 {
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Invalid path format",
		})
		return
	}

	hookID := parts[1]

	// Check if hook exists
	hook, exists := h.storage.GetHook(hookID)
	if !exists {
		respondJSON(w, http.StatusNotFound, map[string]string{
			"error": "Hook not found",
		})
		return
	}

	// Poll interactions (atomic read-and-delete)
	interactions := h.storage.PollInteractions(hookID)

	h.logger.Info("interactions polled",
		"hook_id", hookID,
		"count", len(interactions),
		"client", r.RemoteAddr)

	resp := map[string]interface{}{
		"interactions": interactions,
	}
	// Echo the hook's metadata so a caller can correlate a fired hook back to
	// its injection context without keeping its own registration bookkeeping.
	if hook.Metadata != nil {
		resp["metadata"] = hook.Metadata
	}
	respondJSON(w, http.StatusOK, resp)
}

// HandleActivity handles GET /activity: the long-lived hooks that currently have
// pending interactions. It lets a client discover which of its many long-lived
// hooks have fired without polling each one; the details are then drained via
// GET /poll/:id. It does not mutate state — a hook drops off this list once
// polled.
func (h *APIHandler) HandleActivity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "Method not allowed",
		})
		return
	}

	activity := []storage.HookActivity{}
	if llm, ok := h.storage.(storage.LongLivedManager); ok {
		if found := llm.LongLivedActivity(); found != nil {
			activity = found
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"hooks": activity,
	})
}

// HandleMetrics handles GET /metrics
func (h *APIHandler) HandleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "Method not allowed",
		})
		return
	}

	// Get storage stats
	stats := h.storage.Stats()

	// Get eviction metrics
	evictionMetrics := h.evictor.GetMetrics()

	// Build structured metrics response
	metrics := map[string]interface{}{
		"hooks": map[string]interface{}{
			"active": stats.HooksActive,
		},
		"interactions": map[string]interface{}{
			"total": stats.InteractionsTotal,
			"by_type": map[string]interface{}{
				"dns":  stats.InteractionsDNS,
				"http": stats.InteractionsHTTP,
			},
		},
		"evictions": map[string]interface{}{
			"total": evictionMetrics.EvictionsTTL + evictionMetrics.EvictionsLimit + evictionMetrics.EvictionsMemory + evictionMetrics.EvictionsHookTTL,
			"by_strategy": map[string]interface{}{
				"expired":         evictionMetrics.EvictionsTTL,
				"overflow":        evictionMetrics.EvictionsLimit,
				"memory_pressure": evictionMetrics.EvictionsMemory,
				"hook_expired":    evictionMetrics.EvictionsHookTTL,
			},
		},
		"memory": map[string]interface{}{
			"alloc_mb":      stats.Memory.AllocMB,
			"heap_inuse_mb": stats.Memory.HeapInuseMB,
			"sys_mb":        stats.Memory.SysMB,
			"gc_runs":       stats.Memory.GCRuns,
		},
	}

	respondJSON(w, http.StatusOK, metrics)
}

// respondJSON writes a JSON response
func respondJSON(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	if err := json.NewEncoder(w).Encode(data); err != nil {
		// Can't really handle this error since headers are already written
		return
	}
}

// CaptureHandler handles wildcard HTTP requests
type CaptureHandler struct {
	storage     storage.Manager
	domain      string
	logger      *slog.Logger
	idGenerator func() string
}

// NewCaptureHandler creates a new capture handler
func NewCaptureHandler(storage storage.Manager, domain string, logger *slog.Logger, idGenerator func() string) *CaptureHandler {
	return &CaptureHandler{
		storage:     storage,
		domain:      domain,
		logger:      logger,
		idGenerator: idGenerator,
	}
}

// ServeHTTP handles all wildcard HTTP requests
func (h *CaptureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Extract hook ID from Host header
	host := r.Host
	hookID := h.extractHookID(host)

	if hookID == "" {
		// Not a valid hook subdomain
		w.WriteHeader(http.StatusOK)
		return
	}

	// Read body (with size limit)
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024)) // 10MB limit
	if err != nil {
		h.logger.Error("failed to read request body", "error", err)
		body = []byte{}
	}
	defer r.Body.Close()

	// Extract headers
	headers := make(map[string]string)
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}

	// Create interaction
	sourceIP := netutil.ExtractIP(r.RemoteAddr)
	interaction := storage.HTTPInteraction(
		h.idGenerator(),
		sourceIP,
		r.Method,
		r.URL.Path,
		headers,
		string(body),
	)

	// Store interaction
	h.storage.AddInteraction(hookID, interaction)

	h.logger.Debug("http interaction captured",
		"hook_id", hookID,
		"method", r.Method,
		"path", r.URL.Path,
		"client", sourceIP)

	// Respond with 200 OK
	w.WriteHeader(http.StatusOK)
}

// extractHookID extracts the hook ID from a host header
// Example: abc123.hookd.jomar.ovh -> abc123
func (h *CaptureHandler) extractHookID(host string) string {
	// Remove port if present
	if idx := strings.Index(host, ":"); idx != -1 {
		host = host[:idx]
	}

	// Hostnames are case-insensitive and may arrive in absolute form, so neither
	// varying the casing nor a trailing dot may hide a callback from capture.
	host = strings.ToLower(strings.TrimSuffix(host, "."))

	// Check if it's a subdomain of our domain
	suffix := "." + strings.ToLower(h.domain)
	if !strings.HasSuffix(host, suffix) {
		return ""
	}

	// Extract the subdomain part
	subdomain := strings.TrimSuffix(host, suffix)

	// Handle multi-level subdomains (take the first part). strings.Split always
	// returns at least one element, so parts[0] is safe.
	parts := strings.Split(subdomain, ".")
	return parts[0]
}
