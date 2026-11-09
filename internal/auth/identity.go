package auth

import (
	"errors"
	"sync"
	"time"
)

// This file implements the canonical identity-link reconciliation model
// (AUTH-003). Identity links are keyed on the triple
// (provider_instance, issuer, subject). Email is a verification hint only and
// never merges identities. Linking/unlinking requires recent-auth evidence and
// collisions fail closed into an explicit conflict — no silent merge or delete.

// Typed reconciliation sentinels. Callers match via errors.Is, never string
// matching.
var (
	ErrRecentAuthRequired = errors.New("auth: recent authentication required")
	ErrIdentityCollision  = errors.New("auth: identity link already bound to another identity")
	ErrLinkNotFound       = errors.New("auth: identity link not found")
)

// Identity is a canonical, site-scoped principal. It is the target that
// IdentityLinks resolve to.
type Identity struct {
	ID   string
	Site string
}

// IdentityLink binds one provider identity (provider_instance, issuer, subject)
// to a canonical Identity. Email is retained only as a verification hint; it is
// not a merge key.
type IdentityLink struct {
	ProviderInstanceID string
	Issuer             string
	Subject            string
	IdentityID         string
	Email              string
	LinkedAt           time.Time
}

// linkKey is the canonical triple that identifies a link.
type linkKey struct {
	ProviderInstanceID string
	Issuer             string
	Subject            string
}

func (l IdentityLink) key() linkKey {
	return linkKey{ProviderInstanceID: l.ProviderInstanceID, Issuer: l.Issuer, Subject: l.Subject}
}

// ReconcileAction is the outcome of a link/unlink reconciliation.
type ReconcileAction string

const (
	ActionLinked   ReconcileAction = "linked"
	ActionUnlinked ReconcileAction = "unlinked"
	ActionConflict ReconcileAction = "conflict"
	ActionNoop     ReconcileAction = "noop"
)

// IdentityReconciliation reports what a link/unlink operation did.
type IdentityReconciliation struct {
	Matches []IdentityLink
	Action  ReconcileAction
	Reason  string
}

// IdentityStore is the in-memory reference implementation of the identity-link
// store. The SQL-backed implementation (over _kora_identity_link) must satisfy
// the same semantics; this store is the conformance target.
type IdentityStore struct {
	mu    sync.RWMutex
	byKey map[linkKey]IdentityLink
	byID  map[string][]IdentityLink
}

// NewIdentityStore returns an empty identity-link store.
func NewIdentityStore() *IdentityStore {
	return &IdentityStore{
		byKey: make(map[linkKey]IdentityLink),
		byID:  make(map[string][]IdentityLink),
	}
}

// Link binds link to identityID. recentAuth must be true; otherwise it fails
// with ErrRecentAuthRequired. If the triple is already bound to a different
// identity, it returns ErrIdentityCollision with a conflict reconciliation.
// Re-linking the same triple to the same identity is a no-op.
func (s *IdentityStore) Link(identityID string, link IdentityLink, recentAuth bool) (IdentityReconciliation, error) {
	if !recentAuth {
		return IdentityReconciliation{Action: ActionConflict, Reason: "recent-auth required"}, ErrRecentAuthRequired
	}
	if identityID == "" {
		return IdentityReconciliation{Action: ActionConflict, Reason: "identity required"}, ErrLinkNotFound
	}
	link.IdentityID = identityID
	if link.LinkedAt.IsZero() {
		link.LinkedAt = time.Now().UTC()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.byKey[link.key()]; ok {
		if existing.IdentityID == identityID {
			return IdentityReconciliation{Matches: []IdentityLink{existing}, Action: ActionNoop}, nil
		}
		return IdentityReconciliation{
			Matches: []IdentityLink{existing},
			Action:  ActionConflict,
			Reason:  "triple already bound to another identity",
		}, ErrIdentityCollision
	}

	s.byKey[link.key()] = link
	s.byID[identityID] = append(s.byID[identityID], link)
	return IdentityReconciliation{Matches: []IdentityLink{link}, Action: ActionLinked}, nil
}

// Unlink removes a link for identityID. recentAuth must be true. A link bound
// to a different identity returns ErrLinkNotFound (no cross-identity leak).
func (s *IdentityStore) Unlink(identityID string, key linkKey, recentAuth bool) (IdentityReconciliation, error) {
	if !recentAuth {
		return IdentityReconciliation{Action: ActionConflict, Reason: "recent-auth required"}, ErrRecentAuthRequired
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.byKey[key]
	if !ok || existing.IdentityID != identityID {
		return IdentityReconciliation{}, ErrLinkNotFound
	}
	delete(s.byKey, key)

	links := s.byID[identityID]
	for i, l := range links {
		if l.key() == key {
			links = append(links[:i], links[i+1:]...)
			break
		}
	}
	s.byID[identityID] = links
	return IdentityReconciliation{Matches: []IdentityLink{existing}, Action: ActionUnlinked}, nil
}

// Resolve returns the link for a triple and whether it exists.
func (s *IdentityStore) Resolve(key linkKey) (IdentityLink, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	l, ok := s.byKey[key]
	return l, ok
}

// LinksFor returns the links bound to an identity, in insertion order.
func (s *IdentityStore) LinksFor(identityID string) []IdentityLink {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]IdentityLink, len(s.byID[identityID]))
	copy(out, s.byID[identityID])
	return out
}
