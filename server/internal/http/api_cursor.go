package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jomar/hookd/internal/storage"
)

// AckResult is the per-hook outcome of an acknowledgement.
type AckResult struct {
	Acknowledged int    `json:"acknowledged"`
	Error        string `json:"error,omitempty"`
}

// readCursor reads one hook past a cursor. A store failure is logged and
// reported as an error so the client retries instead of seeing "nothing new".
func (h *APIHandler) readCursor(hookID string, after int64) (*storage.PollResult, int) {
	read, err := h.storage.ReadInteractions(hookID, after)
	if err != nil {
		return &storage.PollResult{Error: h.storeError(err, hookID)}, storeErrorStatus(err)
	}
	return &storage.PollResult{Interactions: read.Interactions, DroppedThrough: &read.DroppedThrough}, http.StatusOK
}

// ackCursor acknowledges one hook through a seq.
func (h *APIHandler) ackCursor(hookID string, through int64) (AckResult, int) {
	removed, err := h.storage.AckInteractions(hookID, through)
	if err != nil {
		return AckResult{Error: h.storeError(err, hookID)}, storeErrorStatus(err)
	}
	return AckResult{Acknowledged: removed}, http.StatusOK
}

// storeError maps a cursor-store error to its client-facing message.
func (h *APIHandler) storeError(err error, hookID string) string {
	if errors.Is(err, storage.ErrHookNotFound) {
		return "Hook not found"
	}
	h.logger.Error("cursor operation failed", "error", err, "hook_id", hookID)
	return "storage error"
}

func storeErrorStatus(err error) int {
	if errors.Is(err, storage.ErrHookNotFound) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// decodeCursors reads a {field: {id: seq}} body for the batch cursor endpoints.
func decodeCursors(w http.ResponseWriter, r *http.Request, field string) (map[string]int64, bool) {
	if r.Method != http.MethodPost {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAPIBodyBytes)

	var req map[string]map[string]int64
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if isTooLarge(err) {
			respondTooLarge(w)
			return nil, false
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return nil, false
	}

	cursors := req[field]
	var msg string
	switch {
	case len(req) != 1 || len(cursors) == 0:
		msg = fmt.Sprintf("body must be {%q: {hook_id: seq}}", field)
	case len(cursors) > maxPollBatch:
		msg = fmt.Sprintf("%s must not exceed %d entries", field, maxPollBatch)
	}
	for _, seq := range cursors {
		if msg == "" && seq < 0 {
			msg = field + " values must not be negative"
		}
	}
	if msg != "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return nil, false
	}
	return cursors, true
}

// HandleRead handles POST /read: {"after": {id: seq}} reads several hooks past
// their cursors without deleting anything.
func (h *APIHandler) HandleRead(w http.ResponseWriter, r *http.Request) {
	cursors, ok := decodeCursors(w, r, "after")
	if !ok {
		return
	}
	results := make(map[string]*storage.PollResult, len(cursors))
	for id, after := range cursors {
		results[id], _ = h.readCursor(id, after)
	}
	h.logger.Info("batch interactions read", "hook_count", len(cursors), "client", r.RemoteAddr)
	respondJSON(w, http.StatusOK, map[string]interface{}{"results": results})
}

// HandleAck handles POST /ack: {"through": {id: seq}} deletes each hook's
// interactions up to its seq, once the client has stored them.
func (h *APIHandler) HandleAck(w http.ResponseWriter, r *http.Request) {
	cursors, ok := decodeCursors(w, r, "through")
	if !ok {
		return
	}
	results := make(map[string]AckResult, len(cursors))
	for id, through := range cursors {
		results[id], _ = h.ackCursor(id, through)
	}
	h.logger.Info("interactions acknowledged", "hook_count", len(cursors), "client", r.RemoteAddr)
	respondJSON(w, http.StatusOK, map[string]interface{}{"results": results})
}

// parseCursor reads a non-negative seq from a query parameter. A present but
// empty value is an error: treating it as absent would drain the hook.
func parseCursor(r *http.Request, name string) (int64, bool, *apiError) {
	query := r.URL.Query()
	if !query.Has(name) {
		return 0, false, nil
	}
	seq, err := strconv.ParseInt(query.Get(name), 10, 64)
	if err != nil || seq < 0 {
		return 0, false, &apiError{http.StatusBadRequest, name + " must be a non-negative integer"}
	}
	return seq, true, nil
}

// ackOne serves DELETE /poll/:id?through=. through is required so a bare
// DELETE cannot wipe a hook by accident.
func (h *APIHandler) ackOne(w http.ResponseWriter, r *http.Request, hookID string) {
	through, ok, errResp := parseCursor(r, "through")
	if errResp == nil && !ok {
		errResp = &apiError{http.StatusBadRequest, "through is required"}
	}
	if errResp != nil {
		respondJSON(w, errResp.status, map[string]string{"error": errResp.message})
		return
	}

	result, status := h.ackCursor(hookID, through)
	if status != http.StatusOK {
		respondJSON(w, status, map[string]string{"error": result.Error})
		return
	}
	h.logger.Info("interactions acknowledged", "hook_id", hookID, "through", through, "count", result.Acknowledged, "client", r.RemoteAddr)
	respondJSON(w, status, result)
}
