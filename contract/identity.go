// Package contract — provider-neutral authentication and identity contracts
// (AUTH-001).
//
// These typed contracts are the foundation the auth epic builds on: every
// sign-in provider implements AuthProvider, every flow returns a single
// IdentityAssertion, and every adapter (HTTP, channel, AI, MCP, SDK) consumes
// that assertion to resolve a principal. Nothing here leaks provider secrets;
// secret values are referenced by opaque identifiers that resolve through the
// secret store.
package contract

import (
	"context"
	"time"
)

// ProviderFamily classifies a sign-in provider family. Families are advertised
// by capability, never inferred from a rendered button.
//
//contract:enum
type ProviderFamily string

const (
	ProviderFamilyOIDC     ProviderFamily = "oidc"
	ProviderFamilySAML     ProviderFamily = "saml"
	ProviderFamilyLDAP     ProviderFamily = "ldap"
	ProviderFamilyWebAuthn ProviderFamily = "webauthn"
	ProviderFamilySocial   ProviderFamily = "social"
	ProviderFamilySCIM     ProviderFamily = "scim"
)

// ProviderProfile is the public capability surface of a provider. It must never
// carry secret references or credentials. Status uses the shared
// CapabilityStatus vocabulary (planned/experimental/supported/retired).
//
//contract:wire
type ProviderProfile struct {
	Family         ProviderFamily   `json:"family"`
	Status         CapabilityStatus `json:"status"`
	Capabilities   []string         `json:"capabilities,omitempty"`
	RequiredChecks []string         `json:"required_checks,omitempty"`
}

// AuthFlow is the intermediate state handed to the client to begin a redirect
// or challenge flow. State is a server-issued, single-use, short-TTL opaque
// token; clients never mint it.
//
//contract:wire
type AuthFlow struct {
	State    string `json:"state"`
	Redirect string `json:"redirect_url,omitempty"`
}

// Claim is one verified, bounded identity claim. Claims are typed and limited;
// raw provider payloads are never propagated.
//
//contract:wire
type Claim struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// IdentityAssertion is the single normalized identity result every provider
// produces. ProviderInstanceID + Issuer + Subject form the canonical identity
// key; Email is a verification hint only, never a merge key.
//
//contract:wire
type IdentityAssertion struct {
	ProviderInstanceID string    `json:"provider_instance_id"`
	Issuer             string    `json:"issuer"`
	Subject            string    `json:"subject"`
	Email              string    `json:"email,omitempty"`
	EmailVerified      bool      `json:"email_verified"`
	FullName           string    `json:"full_name,omitempty"`
	Claims             []Claim   `json:"claims,omitempty"`
	AuthenticatedAt    time.Time `json:"authenticated_at"`
	Amr                string    `json:"amr,omitempty"`
	Acr                string    `json:"acr,omitempty"`
	SessionBinding     string    `json:"session_binding,omitempty"`
}

// PKCEVerifier carries the proof-of-key code exchange material for OAuth2
// authorization-code flows. CodeVerifier is the client secret; CodeChallenge
// is the S256-transformed value sent in the authorization request.
//
//contract:wire
type PKCEVerifier struct {
	CodeVerifier  string
	CodeChallenge string
	Method        string // "S256"
}

// AuthProvider is the provider-neutral sign-in contract. Implementations are
// registered per family and never construct or expose provider secrets.
type AuthProvider interface {
	Discovery() ProviderProfile
	Begin(ctx context.Context, state string) (AuthFlow, error)
	Complete(ctx context.Context, code string, pkce PKCEVerifier) (IdentityAssertion, error)
	Revoke(ctx context.Context, subject string) error
	Health(ctx context.Context) error
}
