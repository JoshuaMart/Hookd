package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jomar/hookd/internal/storage"
)

// HandleRegister handles POST /register.
func (h *APIHandler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}
	owner, secret, errResp := h.registrationOwner(r)
	if errResp != nil {
		respondJSON(w, errResp.status, map[string]string{"error": errResp.message})
		return
	}
	req, errResp := decodeRegistration(w, r)
	if errResp != nil {
		respondJSON(w, errResp.status, map[string]string{"error": errResp.message})
		return
	}
	batch, errResp := h.registrationOptions(req)
	if errResp != nil {
		respondJSON(w, errResp.status, map[string]string{"error": errResp.message})
		return
	}
	for i := range batch {
		batch[i].Owner = owner
	}
	hooks, errResp := h.createHooks(batch)
	if errResp != nil {
		respondJSON(w, errResp.status, map[string]string{"error": errResp.message})
		return
	}
	h.logger.Info("hooks created", withOwner(owner, "count", len(hooks), "client", r.RemoteAddr)...)
	// A generated secret is returned this once; keep it out of caches.
	if secret != "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	// Only legacy single-hook requests return the object directly. A specs
	// request always returns an array, even when it contains a single hook.
	if req.Hooks == nil && len(hooks) == 1 {
		respondJSON(w, http.StatusOK, registeredHook{Hook: hooks[0], Secret: secret})
		return
	}
	resp := map[string]interface{}{"hooks": hooks}
	if secret != "" {
		resp["secret"] = secret
	}
	respondJSON(w, http.StatusOK, resp)
}

// registeredHook is the legacy single-hook response, with a generated secret.
type registeredHook struct {
	*storage.Hook
	Secret string `json:"secret,omitempty"`
}

// decodeRegistration preserves the historical fallback to a single ephemeral
// hook for missing or malformed bodies. Oversized bodies are always rejected.
func decodeRegistration(w http.ResponseWriter, r *http.Request) (registerRequest, *apiError) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAPIBodyBytes)
	var req registerRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			if isTooLarge(err) {
				return registerRequest{}, &apiError{http.StatusRequestEntityTooLarge, fmt.Sprintf("request body must not exceed %d bytes", maxAPIBodyBytes)}
			}
			req = registerRequest{}
		}
	}
	return req, nil
}

// registrationOptions validates the whole request before any hook is created.
func (h *APIHandler) registrationOptions(req registerRequest) ([]storage.CreateOptions, *apiError) {
	if req.Hooks != nil {
		return h.specOptions(req)
	}
	if req.Count < 1 {
		req.Count = 1
	}
	if req.Count > 100 {
		return nil, &apiError{http.StatusBadRequest, "count must not exceed 100"}
	}
	opts, errResp := h.buildCreateOptions(req.TTL, req.Metadata)
	if errResp != nil {
		return nil, errResp
	}
	batch := make([]storage.CreateOptions, req.Count)
	for i := range batch {
		batch[i] = opts
	}
	return batch, nil
}

// maxRegisterSpecs caps registrations carrying per-hook metadata.
const maxRegisterSpecs = 500

func (h *APIHandler) specOptions(req registerRequest) ([]storage.CreateOptions, *apiError) {
	if req.Count != 0 || req.TTL != "" || req.Metadata != nil {
		return nil, &apiError{http.StatusBadRequest, "hooks cannot be combined with count, ttl or metadata"}
	}
	if len(req.Hooks) == 0 || len(req.Hooks) > maxRegisterSpecs {
		return nil, &apiError{http.StatusBadRequest, fmt.Sprintf("hooks must hold 1 to %d entries", maxRegisterSpecs)}
	}
	batch := make([]storage.CreateOptions, len(req.Hooks))
	for i, spec := range req.Hooks {
		opts, errResp := h.buildCreateOptions(spec.TTL, spec.Metadata)
		if errResp != nil {
			return nil, &apiError{errResp.status, fmt.Sprintf("hooks[%d]: %s", i, errResp.message)}
		}
		batch[i] = opts
	}
	return batch, nil
}

// registerRequest is the optional body of POST /register.
type registerRequest struct {
	Count    int            `json:"count,omitempty"`
	TTL      string         `json:"ttl,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Hooks    []hookSpec     `json:"hooks,omitempty"`
}

// hookSpec is one entry of the hooks array of POST /register.
type hookSpec struct {
	TTL      string         `json:"ttl,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// createHooks creates a batch in order. Long-lived hooks go through one
// cap-enforcing transaction first; ephemeral ones cannot fail, so nothing is
// created unless the whole batch can be.
func (h *APIHandler) createHooks(batch []storage.CreateOptions) ([]*storage.Hook, *apiError) {
	// A shared store can fail for either kind of hook: it creates the whole
	// batch in one step and says so.
	if creator, ok := h.storage.(storage.BatchCreator); ok {
		hooks, err := creator.CreateHookBatch(h.domain, batch, h.longLived.MaxHooks)
		if err != nil {
			return nil, h.creationError(err)
		}
		return hooks, nil
	}

	var longLived []storage.CreateOptions
	for _, opts := range batch {
		if opts.TTL > h.evictor.HookTTL() {
			longLived = append(longLived, opts)
		}
	}

	var persisted []*storage.Hook
	if len(longLived) > 0 {
		llm, ok := h.storage.(storage.LongLivedManager)
		if !ok {
			return nil, &apiError{http.StatusBadRequest, "long-lived hooks are disabled"}
		}
		var err error
		persisted, err = llm.CreateLongLivedHooks(h.domain, longLived, h.longLived.MaxHooks)
		if err != nil {
			return nil, h.creationError(err)
		}
	}

	hooks := make([]*storage.Hook, 0, len(batch))
	for _, opts := range batch {
		if opts.TTL > h.evictor.HookTTL() {
			hooks = append(hooks, persisted[0])
			persisted = persisted[1:]
			continue
		}
		hooks = append(hooks, h.storage.CreateHook(h.domain, opts))
	}
	return hooks, nil
}

// creationError maps a storage failure to the registration response.
func (h *APIHandler) creationError(err error) *apiError {
	if errors.Is(err, storage.ErrHookLimitReached) {
		return &apiError{http.StatusTooManyRequests, "long-lived hook limit reached"}
	}
	h.logger.Error("failed to create hooks", "error", err)
	return &apiError{http.StatusInternalServerError, "failed to create hook"}
}
