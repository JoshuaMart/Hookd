package http

import (
	"net/http"
	"strings"

	"github.com/jomar/hookd/internal/storage"
)

// HandleActivity handles GET /activity: the long-lived hooks that currently have
// pending interactions. It lets a client discover which of its many long-lived
// hooks have fired without polling each one; the details are then drained via
// GET /poll/:id. It does not mutate state — a hook drops off this list once
// drained or acknowledged; compare last_seq with a cursor to skip read ones.
// metadata.<key>=<value> query parameters narrow it to matching hooks.
func (h *APIHandler) HandleActivity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "Method not allowed",
		})
		return
	}

	filter := metadataFilter(r)
	activity := []storage.HookActivity{}
	if llm, ok := h.storage.(storage.LongLivedManager); ok {
		for _, a := range llm.LongLivedActivity() {
			if storage.MatchesMetadata(a.Hook.Metadata, filter) {
				activity = append(activity, a)
			}
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"hooks": activity,
	})
}

// HandleHooks handles GET /hooks: every long-lived hook, without interactions,
// optionally narrowed by metadata.<key>=<value>. It lets a client rebuild its
// hook list after losing local state.
func (h *APIHandler) HandleHooks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "Method not allowed",
		})
		return
	}

	filter := metadataFilter(r)
	hooks := []*storage.Hook{}
	if llm, ok := h.storage.(storage.LongLivedManager); ok {
		all, err := llm.LongLivedHooks()
		if err != nil {
			// An empty list would read as "no hooks" to a client recovering state.
			h.logger.Error("failed to list long-lived hooks", "error", err)
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage error"})
			return
		}
		for _, hook := range all {
			if storage.MatchesMetadata(hook.Metadata, filter) {
				hooks = append(hooks, hook)
			}
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"hooks": hooks,
	})
}

// metadataFilter collects the metadata.<key>=<value> query parameters.
func metadataFilter(r *http.Request) map[string]string {
	filter := map[string]string{}
	for name, values := range r.URL.Query() {
		if key, ok := strings.CutPrefix(name, "metadata."); ok && key != "" && len(values) > 0 {
			filter[key] = values[0]
		}
	}
	return filter
}
