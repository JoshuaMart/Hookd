package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// maxPollBatch caps the hook IDs accepted by POST /poll.
const maxPollBatch = 1000

// HandlePollBatch handles POST /poll (batch polling)
func (h *APIHandler) HandlePollBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "Method not allowed",
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAPIBodyBytes)

	// Parse request body as array of hook IDs
	var hookIDs []string

	if err := json.NewDecoder(r.Body).Decode(&hookIDs); err != nil {
		if isTooLarge(err) {
			respondTooLarge(w)
			return
		}
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

	// The result map is sized to the request, so the batch is bounded too.
	if len(hookIDs) > maxPollBatch {
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("hook_ids must not exceed %d entries", maxPollBatch),
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

// HandlePoll handles GET /poll/:id (drain, or read past ?after=) and
// DELETE /poll/:id?through= (acknowledge).
func (h *APIHandler) HandlePoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
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

	if r.Method == http.MethodDelete {
		h.ackOne(w, r, hookID)
		return
	}

	after, cursor, errResp := parseCursor(r, "after")
	if errResp != nil {
		respondJSON(w, errResp.status, map[string]string{"error": errResp.message})
		return
	}

	// Check if hook exists
	hook, exists := h.storage.GetHook(hookID)
	if !exists {
		respondJSON(w, http.StatusNotFound, map[string]string{
			"error": "Hook not found",
		})
		return
	}

	resp := map[string]interface{}{}
	if cursor {
		read, status := h.readCursor(hookID, after)
		if status != http.StatusOK {
			respondJSON(w, status, map[string]string{"error": read.Error})
			return
		}
		resp["interactions"] = read.Interactions
		resp["dropped_through"] = read.DroppedThrough
		h.logger.Info("interactions read", "hook_id", hookID, "after", after, "count", len(read.Interactions), "client", r.RemoteAddr)
	} else {
		// Atomic read-and-delete
		interactions := h.storage.PollInteractions(hookID)
		resp["interactions"] = interactions
		h.logger.Info("interactions polled", "hook_id", hookID, "count", len(interactions), "client", r.RemoteAddr)
	}
	// Echo the hook's metadata so a caller can correlate a fired hook back to
	// its injection context without keeping its own registration bookkeeping.
	if hook.Metadata != nil {
		resp["metadata"] = hook.Metadata
	}
	respondJSON(w, http.StatusOK, resp)
}
