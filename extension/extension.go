// Package extension owns installed tenant extensions and their API
// credentials. Records here are persistence models; transports decide what to
// expose and must never return stored secrets or token verifiers.
package extension

import (
	"errors"
	"time"
)

// ErrNotFound reports that no extension or active credential matched.
var ErrNotFound = errors.New("extension not found")

// Credential is the active extension resolved from an access token verifier.
type Credential struct {
	Name           string
	APIPermissions string
}

// Summary is the administrator listing view of an installed extension.
type Summary struct {
	Name                string
	DisplayName         string
	Description         string
	EndpointURL         string
	IsActive            bool
	Subscriptions       string
	APIPermissions      string
	SecretCount         int
	ConsecutiveFailures int
	InstalledAt         string
	LastDeliveryAt      string
	LastError           string
}

// Registration is a new extension. AccessTokenHash is the stored verifier,
// never the bearer token itself.
type Registration struct {
	Site            string
	Name            string
	DisplayName     string
	Description     string
	EndpointURL     string
	Secret          string
	AccessTokenHash string
	Subscriptions   string
	APIPermissions  string
}

// SecretRotation replaces the signing secret while the previous one stays
// valid until PreviousExpiresAt.
type SecretRotation struct {
	Site              string
	Name              string
	Secret            string
	PreviousExpiresAt time.Time
}
