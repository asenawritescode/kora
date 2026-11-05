package auth

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashExtensionAccessToken returns the stored verifier for an extension or
// managed channel bearer token. Tokens are 256-bit random values, so an
// unsalted SHA-256 digest is sufficient and keeps lookups indexable.
func HashExtensionAccessToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
