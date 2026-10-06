package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// maxAPIBodyBytes caps the request body of the authenticated endpoints.
const maxAPIBodyBytes = 1 << 20 // 1 MiB

// apiError bundles an HTTP status with a client-facing message.
type apiError struct {
	status  int
	message string
}

// isTooLarge distinguishes an over-limit body from a malformed one.
func isTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

// respondTooLarge rejects an oversized request body.
func respondTooLarge(w http.ResponseWriter) {
	respondJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
		"error": fmt.Sprintf("request body must not exceed %d bytes", maxAPIBodyBytes),
	})
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
