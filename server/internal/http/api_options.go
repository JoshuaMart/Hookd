package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jomar/hookd/internal/storage"
)

// defaultMaxMetadataBytes bounds hook metadata when the config value is unset.
const defaultMaxMetadataBytes = 8192

// buildCreateOptions validates the optional ttl and metadata and returns the
// resolved storage options. A nil apiError means success.
//
// TTL semantics: absent means ephemeral (the configured hook TTL). A value at or
// below the ephemeral hook TTL is rejected — ephemeral hooks are requested by
// omitting ttl. A value above it designates a long-lived hook, capped at
// long_lived.max_ttl, and requires the long-lived store to be enabled.
func (h *APIHandler) buildCreateOptions(ttl string, metadata map[string]any) (storage.CreateOptions, *apiError) {
	opts := storage.CreateOptions{Metadata: metadata, SMTPEnabled: h.smtpEnabled}

	if errResp := h.validateMetadata(metadata); errResp != nil {
		return opts, errResp
	}
	duration, errResp := h.resolveTTL(ttl)
	if errResp == nil {
		opts.TTL = duration
	}
	return opts, errResp
}

// validateMetadata applies the same size cap to ephemeral and long-lived hooks.
func (h *APIHandler) validateMetadata(metadata map[string]any) *apiError {
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
			return &apiError{http.StatusBadRequest, "invalid metadata"}
		}
		if len(encoded) > limit {
			return &apiError{http.StatusBadRequest, fmt.Sprintf("metadata must not exceed %d bytes", limit)}
		}
	}

	return nil
}

// resolveTTL applies the lifetime policy independently of metadata validation.
func (h *APIHandler) resolveTTL(ttl string) (time.Duration, *apiError) {
	ephemeralTTL := h.evictor.HookTTL()
	if ttl == "" {
		return ephemeralTTL, nil
	}

	d, err := parseTTL(ttl)
	if err != nil {
		return 0, &apiError{http.StatusBadRequest, "invalid ttl (use a Go duration like \"168h\" or a day count like \"7d\")"}
	}
	if d <= ephemeralTTL {
		return 0, &apiError{http.StatusBadRequest, fmt.Sprintf("ttl must exceed the ephemeral hook ttl (%s); omit ttl for ephemeral hooks", ephemeralTTL)}
	}
	if !h.longLived.Enabled {
		return 0, &apiError{http.StatusBadRequest, "long-lived hooks are disabled"}
	}
	if d > h.longLived.MaxTTL {
		return 0, &apiError{http.StatusBadRequest, fmt.Sprintf("ttl must not exceed %s", h.longLived.MaxTTL)}
	}
	return d, nil
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
