package oidc

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

// State is the server-side record bound to an OIDC authorization request. It
// binds the opaque token to a site and a nonce so a captured callback cannot
// be replayed or used on a different site.
type State struct {
	Token     string
	Site      string
	Nonce     string
	CreatedAt time.Time
	used      bool
}

// StateStore is an in-memory, single-use, TTL-bounded store for OIDC state.
// The production flow will persist state (so it survives restart) or use a
// short-TTL signed token; this reference store is the concurrency-safe core.
type StateStore struct {
	mu     sync.Mutex
	states map[string]*State
	ttl    time.Duration
}

// NewStateStore returns a store with the given TTL.
func NewStateStore(ttl time.Duration) *StateStore {
	return &StateStore{states: make(map[string]*State), ttl: ttl}
}

// New creates an opaque, site-bound state token and records it with nonce.
func (s *StateStore) New(site, nonce string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[token] = &State{
		Token:     token,
		Site:      site,
		Nonce:     nonce,
		CreatedAt: time.Now().UTC(),
	}
	return token, nil
}

// Consume verifies and atomically consumes a state token for the given site,
// returning the nonce. It fails closed on a missing/expired/tampered token, a
// site mismatch, or a replay (already consumed).
func (s *StateStore) Consume(token, site string) (nonce string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[token]
	if !ok || st == nil {
		return "", ErrStateInvalid
	}
	if st.used {
		return "", ErrStateReplayed
	}
	if time.Since(st.CreatedAt) > s.ttl {
		return "", ErrStateExpired
	}
	if subtle.ConstantTimeCompare([]byte(st.Site), []byte(site)) != 1 {
		return "", ErrStateSiteMismatch
	}
	st.used = true
	return st.Nonce, nil
}

var (
	// ErrStateInvalid is returned for an unknown/tampered state token.
	ErrStateInvalid = errors.New("oidc: invalid state")
	// ErrStateReplayed is returned when a state token is used a second time.
	ErrStateReplayed = errors.New("oidc: state already used")
	// ErrStateExpired is returned when a state token exceeds its TTL.
	ErrStateExpired = errors.New("oidc: state expired")
	// ErrStateSiteMismatch is returned when state is redeemed for another site.
	ErrStateSiteMismatch = errors.New("oidc: state site mismatch")
)
