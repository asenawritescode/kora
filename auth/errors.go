package auth

import "errors"

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrSessionExpired     = errors.New("session expired")
	ErrDisabledAccount    = errors.New("account disabled")
	ErrNoDBConnection     = errors.New("no database connection available")
)
