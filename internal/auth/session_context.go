package auth

import (
	"context"
	"time"
)

// sessionCreatedAtKey is the unexported context key under which the creation time of the
// authenticated session travels with a request. Using a private type means no other package can
// collide with, or forge, the value.
type sessionCreatedAtKey struct{}

// ContextWithSessionCreatedAt returns a context that carries when the caller's session was created,
// so downstream code can require a recent sign-in for sensitive operations.
func ContextWithSessionCreatedAt(ctx context.Context, createdAt time.Time) context.Context {
	return context.WithValue(ctx, sessionCreatedAtKey{}, createdAt)
}

// SessionCreatedAt returns the session creation time stored by ContextWithSessionCreatedAt.
func SessionCreatedAt(ctx context.Context) (time.Time, bool) {
	createdAt, ok := ctx.Value(sessionCreatedAtKey{}).(time.Time)
	return createdAt, ok
}
