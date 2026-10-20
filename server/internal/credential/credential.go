// Package credential derives the stored and logged forms of a secret.
package credential

import (
	"crypto/sha256"
	"encoding/hex"
)

// Hash is the hex SHA-256 under which a secret is stored. A fast hash
// suffices for a high-entropy secret.
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Fingerprint names a secret in logs without disclosing it.
func Fingerprint(secret string) string { return HashFingerprint(Hash(secret)) }

// HashFingerprint is the Fingerprint of a secret known only by its Hash.
func HashFingerprint(hash string) string { return hash[:8] }
