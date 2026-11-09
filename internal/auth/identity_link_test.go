package auth

import (
	"errors"
	"testing"
)

func link(provider, issuer, subject, email string) IdentityLink {
	return IdentityLink{ProviderInstanceID: provider, Issuer: issuer, Subject: subject, Email: email}
}

func TestLinkRequiresRecentAuth(t *testing.T) {
	s := NewIdentityStore()
	if _, err := s.Link("id-a", link("oidc:entra", "iss", "s1", ""), false); !errors.Is(err, ErrRecentAuthRequired) {
		t.Fatalf("want ErrRecentAuthRequired, got %v", err)
	}
	if _, err := s.Unlink("id-a", link("oidc:entra", "iss", "s1", "").key(), false); !errors.Is(err, ErrRecentAuthRequired) {
		t.Fatalf("unlink want ErrRecentAuthRequired, got %v", err)
	}
}

func TestLinkAndResolveByTriple(t *testing.T) {
	s := NewIdentityStore()
	l := link("oidc:entra", "https://issuer", "subj-1", "u@acme.com")
	if _, err := s.Link("id-a", l, true); err != nil {
		t.Fatalf("link: %v", err)
	}
	got, ok := s.Resolve(l.key())
	if !ok || got.IdentityID != "id-a" {
		t.Fatalf("resolve = %+v ok=%v", got, ok)
	}
}

func TestEmailAloneNeverMerges(t *testing.T) {
	s := NewIdentityStore()
	// Same email, two different provider subjects, two different identities.
	_ = mustLink(t, s, "id-a", link("oidc:entra", "https://issuer", "subj-1", "shared@acme.com"))
	_ = mustLink(t, s, "id-b", link("oidc:entra", "https://issuer", "subj-2", "shared@acme.com"))

	// Resolving subj-2 must yield id-b, never id-a.
	got, ok := s.Resolve(link("oidc:entra", "https://issuer", "subj-2", "").key())
	if !ok || got.IdentityID != "id-b" {
		t.Fatalf("email caused an unintended merge: got %+v", got)
	}
	if len(s.LinksFor("id-a")) != 1 || len(s.LinksFor("id-b")) != 1 {
		t.Fatalf("links unexpectedly shared across identities")
	}
}

func TestCollisionProducesConflictNotSilentMerge(t *testing.T) {
	s := NewIdentityStore()
	_ = mustLink(t, s, "id-a", link("oidc:entra", "https://issuer", "subj-1", ""))

	rec, err := s.Link("id-b", link("oidc:entra", "https://issuer", "subj-1", ""), true)
	if !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("want ErrIdentityCollision, got %v", err)
	}
	if rec.Action != ActionConflict {
		t.Fatalf("action = %q, want conflict", rec.Action)
	}
	if len(rec.Matches) != 1 || rec.Matches[0].IdentityID != "id-a" {
		t.Fatalf("conflict should report the existing link: %+v", rec.Matches)
	}
	// The existing link must be untouched.
	if got, _ := s.Resolve(link("oidc:entra", "https://issuer", "subj-1", "").key()); got.IdentityID != "id-a" {
		t.Fatalf("collision mutated existing link: %+v", got)
	}
}

func TestRelinkSameTripleSameIdentityIsNoop(t *testing.T) {
	s := NewIdentityStore()
	_ = mustLink(t, s, "id-a", link("oidc:entra", "iss", "s1", ""))
	rec, err := s.Link("id-a", link("oidc:entra", "iss", "s1", ""), true)
	if err != nil || rec.Action != ActionNoop {
		t.Fatalf("want noop, got action=%q err=%v", rec.Action, err)
	}
}

func TestUnlinkRevokesDerivedLink(t *testing.T) {
	s := NewIdentityStore()
	l := link("oidc:entra", "iss", "s1", "")
	_ = mustLink(t, s, "id-a", l)

	rec, err := s.Unlink("id-a", l.key(), true)
	if err != nil || rec.Action != ActionUnlinked {
		t.Fatalf("unlink: action=%q err=%v", rec.Action, err)
	}
	if _, ok := s.Resolve(l.key()); ok {
		t.Fatalf("link still resolvable after unlink")
	}
	if len(s.LinksFor("id-a")) != 0 {
		t.Fatalf("identity still holds links after unlink")
	}
}

func TestUnlinkCrossIdentityDoesNotLeak(t *testing.T) {
	s := NewIdentityStore()
	l := link("oidc:entra", "iss", "s1", "")
	_ = mustLink(t, s, "id-a", l)
	if _, err := s.Unlink("id-b", l.key(), true); !errors.Is(err, ErrLinkNotFound) {
		t.Fatalf("want ErrLinkNotFound for cross-identity unlink, got %v", err)
	}
	if _, ok := s.Resolve(l.key()); !ok {
		t.Fatalf("cross-identity unlink removed a link it did not own")
	}
}

func mustLink(t *testing.T, s *IdentityStore, id string, l IdentityLink) IdentityReconciliation {
	t.Helper()
	rec, err := s.Link(id, l, true)
	if err != nil {
		t.Fatalf("link(%s,%s): %v", id, l.Subject, err)
	}
	return rec
}
