package http

import (
	"encoding/json"
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

	// Parse request body (optional)
	var req struct {
		Count    int            `json:"count,omitempty"`
		TTL      string         `json:"ttl,omitempty"`
		Metadata map[string]any `json:"metadata,omitempty"`
	}

	// Only parse body if it exists. A malformed body falls back to a single
	// ephemeral hook, preserving the historical lenient behavior.
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			req = struct {
				Count    int            `json:"count,omitempty"`
				TTL      string         `json:"ttl,omitempty"`
				Metadata map[string]any `json:"metadata,omitempty"`
			}{}
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

	// Long-lived registrations are bounded so the persistent store cannot grow
	// without limit.
	if opts.TTL > h.evictor.HookTTL() {
		if llm, ok := h.storage.(storage.LongLivedManager); ok {
			if llm.LongLivedCount()+req.Count > h.longLived.MaxHooks {
				respondJSON(w, http.StatusTooManyRequests, map[string]string{
					"error": "long-lived hook limit reached",
				})
				return
			}
		}
	}

	// Single hook case
	if req.Count == 1 {
		hook := h.storage.CreateHook(h.domain, opts)
		h.logger.Info("hook created", "id", hook.ID, "long_lived", opts.TTL > h.evictor.HookTTL(), "client", r.RemoteAddr)
		respondJSON(w, http.StatusOK, hook)
		return
	}

	// Multiple hooks case
	hooks := make([]interface{}, req.Count)
	for i := 0; i < req.Count; i++ {
		hook := h.storage.CreateHook(h.domain, opts)
		hooks[i] = hook
		h.logger.Debug("hook created", "id", hook.ID, "index", i+1, "total", req.Count, "client", r.RemoteAddr)
	}

	h.logger.Info("hooks created", "count", req.Count, "long_lived", opts.TTL > h.evictor.HookTTL(), "client", r.RemoteAddr)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"hooks": hooks,
	})
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

	if metadata != nil && h.longLived.MaxMetadataBytes > 0 {
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return opts, &apiError{http.StatusBadRequest, "invalid metadata"}
		}
		if len(encoded) > h.longLived.MaxMetadataBytes {
			return opts, &apiError{http.StatusBadRequest, fmt.Sprintf("metadata must not exceed %d bytes", h.longLived.MaxMetadataBytes)}
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

// parseTTL accepts a Go duration ("168h", "90m") or a plain day count ("7d").
func parseTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("invalid day count: %q", s)
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

	// Check if it's a subdomain of our domain
	suffix := "." + h.domain
	if !strings.HasSuffix(host, suffix) {
		// Check if it's the exact domain (no subdomain)
		if host == h.domain {
			return ""
		}
		return ""
	}

	// Extract the subdomain part
	subdomain := strings.TrimSuffix(host, suffix)

	// Handle multi-level subdomains (take the first part). strings.Split always
	// returns at least one element, so parts[0] is safe.
	parts := strings.Split(subdomain, ".")
	return parts[0]
}
