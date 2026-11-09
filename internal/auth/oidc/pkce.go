// Package oidc implements the OIDC authorization-code + PKCE profile
// (AUTH-002). The validation primitives here are pure and unit-testable; the
// network flow (discovery, token exchange, JWKS signature verification) is
// layered on top and remains the caller's responsibility.
package oidc

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"github.com/asenawritescode/kora/contract"
)

// ErrPKCEMismatch is returned when a code verifier does not match its
// challenge. It is a typed sentinel; callers must not string-match.
var ErrPKCEMismatch = errors.New("oidc: PKCE code verifier does not match challenge")

// GeneratePKCE creates a new PKCE pair using the S256 method: a random
// 32-byte verifier and its base64url SHA-256 challenge.
func GeneratePKCE() (contract.PKCEVerifier, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return contract.PKCEVerifier{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	return contract.PKCEVerifier{
		CodeVerifier:  verifier,
		CodeChallenge: S256Challenge(verifier),
		Method:        "S256",
	}, nil
}

// S256Challenge returns the base64url-encoded SHA-256 of verifier, per
// RFC 7636 §4.2.
func S256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// VerifyPKCE proves that verifier derives challenge. A mismatch fails closed.
func VerifyPKCE(verifier, challenge string) error {
	if S256Challenge(verifier) != challenge {
		return ErrPKCEMismatch
	}
	return nil
}
