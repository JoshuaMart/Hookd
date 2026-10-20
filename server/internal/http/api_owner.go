package http

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"

	"github.com/jomar/hookd/internal/storage"
)

// ownerSecretHeader carries the owner secret. It is never read from the query
// string, which ends up in access logs.
const ownerSecretHeader = "X-Hookd-Secret"

// A client secret must be long enough to hold 128 bits as hex.
const (
	minSecretLen         = 32
	maxSecretLen         = 256
	generatedSecretBytes = 32
)

var (
	errMissingSecret = &apiError{http.StatusUnauthorized, "Invalid or missing owner secret"}
	errInvalidSecret = &apiError{http.StatusBadRequest,
		fmt.Sprintf("%s must be %d to %d visible ASCII characters", ownerSecretHeader, minSecretLen, maxSecretLen)}
)

// validSecret accepts 32 to 256 visible ASCII characters.
func validSecret(secret string) bool {
	if len(secret) < minSecretLen || len(secret) > maxSecretLen {
		return false
	}
	for i := 0; i < len(secret); i++ {
		if secret[i] < 0x21 || secret[i] > 0x7e {
			return false
		}
	}
	return true
}

// hashSecret derives the stored owner. A fast hash suffices for a secret of
// this entropy.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// generateSecret returns 256 random bits as hex.
func generateSecret() string {
	b := make([]byte, generatedSecretBytes)
	_, _ = rand.Read(b) // never fails since Go 1.24
	return hex.EncodeToString(b)
}

// requestOwner returns the owner a request acts for, AnyOwner when owner
// secrets are disabled. It does not depend on any hook, so it cannot reveal one.
func (h *APIHandler) requestOwner(r *http.Request) (string, *apiError) {
	if !h.ownerSecret {
		return storage.AnyOwner, nil
	}
	secret := r.Header.Get(ownerSecretHeader)
	if secret == "" {
		return "", errMissingSecret
	}
	if !validSecret(secret) {
		return "", errInvalidSecret
	}
	return hashSecret(secret), nil
}

// registrationOwner is requestOwner for POST /register, where a missing secret
// is generated and returned once.
func (h *APIHandler) registrationOwner(r *http.Request) (owner, generated string, errResp *apiError) {
	if h.ownerSecret && r.Header.Get(ownerSecretHeader) == "" {
		generated = generateSecret()
		return hashSecret(generated), generated, nil
	}
	owner, errResp = h.requestOwner(r)
	return owner, "", errResp
}

// ownerOrReject resolves the request's owner, answering the error itself.
func (h *APIHandler) ownerOrReject(w http.ResponseWriter, r *http.Request) (string, bool) {
	owner, errResp := h.requestOwner(r)
	if errResp != nil {
		respondJSON(w, errResp.status, map[string]string{"error": errResp.message})
		return "", false
	}
	return owner, true
}

// withOwner appends a short owner fingerprint to log attributes, like the
// token fingerprint; the secret and the full hash are never logged.
func withOwner(owner string, attrs ...any) []any {
	if owner == storage.AnyOwner {
		return attrs
	}
	return append(attrs, "owner", owner[:8])
}
