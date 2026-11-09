package oidc

import (
	"errors"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
)

// IDTokenClaims is the decoded, signature-verified claim set of an OIDC ID
// token. Signature verification (JWKS) happens before this struct is produced;
// this package validates the claims and maps them to an IdentityAssertion.
type IDTokenClaims struct {
	Issuer        string
	Subject       string
	Audience      string
	Email         string
	EmailVerified bool
	Name          string
	ExpiresAt     time.Time
	Nonce         string
	Groups        []string
}

// ValidateOptions carries the expected values and JIT policy for claim
// validation.
type ValidateOptions struct {
	ExpectedIssuer   string
	ExpectedAudience string
	ExpectedNonce    string
	Now              time.Time
	AllowedDomains   []string
	AllowedGroups    []string
	JITProvision     bool
}

// JWKSKey represents a verification key in a JWKS document. The verifier uses
// KeyID to select the active key after rotation; callers must fail closed when
// the kid is missing or stale.
type JWKSKey struct {
	KeyID   string
	Issuer  string
	Active  bool
	Retired bool
}

// Typed validation sentinels. Callers match via errors.Is, never string
// matching.
var (
	ErrIssuerMismatch   = errors.New("oidc: issuer mismatch")
	ErrAudienceMismatch = errors.New("oidc: audience mismatch")
	ErrTokenExpired     = errors.New("oidc: token expired")
	ErrNonceMismatch    = errors.New("oidc: nonce mismatch")
	ErrJITDomainDenied  = errors.New("oidc: email domain not allowed")
	ErrJITGroupDenied   = errors.New("oidc: group not allowed")
	ErrKeyNotFound      = errors.New("oidc: jwks key not found")
	ErrKeyRetired       = errors.New("oidc: jwks key retired")
)

// ValidateIDToken validates claims against opts and, on success, returns the
// normalized IdentityAssertion. Every check fails closed.
func ValidateIDToken(c IDTokenClaims, opts ValidateOptions) (*contract.IdentityAssertion, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	if opts.ExpectedIssuer != "" && c.Issuer != opts.ExpectedIssuer {
		return nil, ErrIssuerMismatch
	}
	if opts.ExpectedAudience != "" && c.Audience != opts.ExpectedAudience {
		return nil, ErrAudienceMismatch
	}
	if !c.ExpiresAt.IsZero() && !opts.Now.Before(c.ExpiresAt) {
		return nil, ErrTokenExpired
	}
	if opts.ExpectedNonce != "" && c.Nonce != opts.ExpectedNonce {
		return nil, ErrNonceMismatch
	}

	// JIT provisioning is deny-by-default: an allow-list entry must match, and
	// a mismatch rejects rather than falling back to open provisioning.
	if opts.JITProvision {
		if len(opts.AllowedDomains) > 0 && !domainAllowed(emailDomain(c.Email), opts.AllowedDomains) {
			return nil, ErrJITDomainDenied
		}
		if len(opts.AllowedGroups) > 0 && !intersects(c.Groups, opts.AllowedGroups) {
			return nil, ErrJITGroupDenied
		}
	}

	return &contract.IdentityAssertion{
		Issuer:          c.Issuer,
		Subject:         c.Subject,
		Email:           c.Email,
		EmailVerified:   c.EmailVerified,
		FullName:        c.Name,
		AuthenticatedAt: opts.Now.UTC(),
	}, nil
}

// SelectJWKSKey chooses the active verification key for a token kid. It fails
// closed when the key is missing, inactive, or retired. Callers can use this to
// keep key rotation deterministic across stale JWKS caches.
func SelectJWKSKey(keys []JWKSKey, kid string, issuer string) (JWKSKey, error) {
	if strings.TrimSpace(kid) == "" {
		return JWKSKey{}, ErrKeyNotFound
	}
	for _, key := range keys {
		if key.KeyID != kid {
			continue
		}
		if issuer != "" && key.Issuer != "" && key.Issuer != issuer {
			return JWKSKey{}, ErrIssuerMismatch
		}
		if key.Retired {
			return JWKSKey{}, ErrKeyRetired
		}
		if !key.Active {
			return JWKSKey{}, ErrKeyNotFound
		}
		return key, nil
	}
	return JWKSKey{}, ErrKeyNotFound
}

func emailDomain(email string) string {
	if i := strings.LastIndex(email, "@"); i >= 0 {
		return strings.ToLower(email[i+1:])
	}
	return ""
}

func domainAllowed(domain string, allowed []string) bool {
	domain = strings.ToLower(domain)
	for _, a := range allowed {
		if strings.EqualFold(a, domain) {
			return true
		}
	}
	return false
}

func intersects(a, b []string) bool {
	set := make(map[string]bool, len(b))
	for _, v := range b {
		set[v] = true
	}
	for _, v := range a {
		if set[v] {
			return true
		}
	}
	return false
}
