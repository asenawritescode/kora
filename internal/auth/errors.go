package auth

import "errors"

var (
	ErrInvalidCredentials        = errors.New("invalid credentials")
	ErrSessionExpired            = errors.New("session expired")
	ErrDisabledAccount           = errors.New("account disabled")
	ErrEmailVerificationRequired = errors.New("email verification required")
	ErrNoDBConnection            = errors.New("no database connection available")
	ErrMagicLinkExpired          = errors.New("magic link expired")
	ErrMagicLinkUsed             = errors.New("magic link already used")
	ErrMagicLinkRevoked          = errors.New("magic link revoked")
	ErrAuthRateLimited           = errors.New("authentication rate limited")
)
