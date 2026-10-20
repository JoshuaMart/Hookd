package http

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"

	"github.com/jomar/hookd/internal/credential"
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

// generateSecret returns 256 random bits as hex.
func generateSecret() string {
	b := make([]byte, generatedSecretBytes)
	_, _ = rand.Read(b) // never fails since Go 1.24
	return hex.EncodeToString(b)
}

// requestScope returns the hooks a request may reach: every hook when owner
// secrets are disabled. It does not depend on any hook, so it cannot reveal one.
func (h *APIHandler) requestScope(r *http.Request) (storage.Scope, *apiError) {
	if !h.ownerSecret {
		return storage.AllOwners(), nil
	}
	secret := r.Header.Get(ownerSecretHeader)
	if secret == "" {
		return storage.Scope{}, errMissingSecret
	}
	if !validSecret(secret) {
		return storage.Scope{}, errInvalidSecret
	}
	return storage.OwnerScope(credential.Hash(secret)), nil
}

// registrationOwner returns the owner new hooks get, empty when owner secrets
// are disabled. A missing secret is generated and returned once.
func (h *APIHandler) registrationOwner(r *http.Request) (owner, generated string, errResp *apiError) {
	if !h.ownerSecret {
		return "", "", nil
	}
	secret := r.Header.Get(ownerSecretHeader)
	if secret == "" {
		generated = generateSecret()
		return credential.Hash(generated), generated, nil
	}
	if !validSecret(secret) {
		return "", "", errInvalidSecret
	}
	return credential.Hash(secret), "", nil
}

// scopeOrReject resolves the request's scope, answering the error itself.
func (h *APIHandler) scopeOrReject(w http.ResponseWriter, r *http.Request) (storage.Scope, bool) {
	scope, errResp := h.requestScope(r)
	if errResp != nil {
		respondJSON(w, errResp.status, map[string]string{"error": errResp.message})
		return storage.Scope{}, false
	}
	return scope, true
}

// withOwner appends the owner's fingerprint to log attributes, like the token
// fingerprint; the secret and the full hash are never logged.
func withOwner(owner string, attrs ...any) []any {
	if owner == "" {
		return attrs
	}
	return append(attrs, "owner", credential.HashFingerprint(owner))
}
