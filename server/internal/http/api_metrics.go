package http

import (
	"net/http"
)

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
				"smtp": stats.InteractionsSMTP,
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
