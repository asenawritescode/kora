package auth

import (
	"context"
	"testing"
	"time"
)

func TestSessionCreatedAtRoundTrip(t *testing.T) {
	want := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	got, ok := SessionCreatedAt(ContextWithSessionCreatedAt(context.Background(), want))
	if !ok || !got.Equal(want) {
		t.Fatalf("SessionCreatedAt() = %v, %v; want %v, true", got, ok, want)
	}
}

func TestSessionCreatedAtAbsentAndNotForgeableByStringKey(t *testing.T) {
	if _, ok := SessionCreatedAt(context.Background()); ok {
		t.Fatal("SessionCreatedAt() reported a value on an empty context")
	}
	// A plain string key, as older code used, must not satisfy the typed accessor.
	forged := context.WithValue(context.Background(), "session_created_at", time.Now()) //nolint:staticcheck // asserting that a bare string key is ignored
	if _, ok := SessionCreatedAt(forged); ok {
		t.Fatal("SessionCreatedAt() accepted a bare string key")
	}
}
