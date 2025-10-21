package http

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jomar/hookd/internal/eviction"
	"github.com/jomar/hookd/internal/storage"
)

// APIHandler handles API endpoints
type APIHandler struct {
	storage     storage.Manager
	evictor     *eviction.Evictor
	domain      string
	logger      *slog.Logger
	idGenerator func() string
}

// NewAPIHandler creates a new API handler
func NewAPIHandler(storage storage.Manager, evictor *eviction.Evictor, domain string, logger *slog.Logger, idGenerator func() string) *APIHandler {
	return &APIHandler{
		storage:     storage,
		evictor:     evictor,
		domain:      domain,
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
		Count int `json:"count,omitempty"`
	}

	// Only parse body if content-type is JSON and body exists
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			// If body parsing fails, treat as count=1
			req.Count = 1
		}
	}

	// Default to 1 if count not specified or invalid
	if req.Count < 1 {
		req.Count = 1
	}

	// Single hook case
	if req.Count == 1 {
		hook := h.storage.CreateHook(h.domain)
		h.logger.Info("hook created", "id", hook.ID, "client", r.RemoteAddr)
		respondJSON(w, http.StatusOK, hook)
		return
	}

	// Multiple hooks case
	hooks := make([]interface{}, req.Count)
	for i := 0; i < req.Count; i++ {
		hook := h.storage.CreateHook(h.domain)
		hooks[i] = hook
		h.logger.Debug("hook created", "id", hook.ID, "index", i+1, "total", req.Count, "client", r.RemoteAddr)
	}

	h.logger.Info("hooks created", "count", req.Count, "client", r.RemoteAddr)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"hooks": hooks,
	})
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
	if _, exists := h.storage.GetHook(hookID); !exists {
		respondJSON(w, http.StatusNotFound, map[string]string{
			"error": "Hook not found",
		})
		return
	}

	// Poll interactions (atomic read-and-delete)
	interactions, err := h.storage.PollInteractions(hookID)
	if err != nil {
		h.logger.Error("failed to poll interactions", "error", err, "hook_id", hookID)
		respondJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "Internal server error",
		})
		return
	}

	h.logger.Info("interactions polled",
		"hook_id", hookID,
		"count", len(interactions),
		"client", r.RemoteAddr)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"interactions": interactions,
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
	sourceIP := extractIP(r.RemoteAddr)
	interaction := storage.HTTPInteraction(
		h.idGenerator(),
		sourceIP,
		r.Method,
		r.URL.Path,
		headers,
		string(body),
	)

	// Store interaction
	if err := h.storage.AddInteraction(hookID, interaction); err != nil {
		h.logger.Error("failed to store http interaction", "error", err)
	}

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

	// Handle multi-level subdomains (take the first part)
	parts := strings.Split(subdomain, ".")
	if len(parts) == 0 {
		return ""
	}

	return parts[0]
}

// extractIP extracts the IP address from a remote address string
func extractIP(remoteAddr string) string {
	if idx := strings.LastIndex(remoteAddr, ":"); idx != -1 {
		return remoteAddr[:idx]
	}
	return remoteAddr
}
