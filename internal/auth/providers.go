package auth

import (
	"fmt"
	"sort"

	"github.com/asenawritescode/kora/contract"
)

// AuthProvider is the public discovery projection for a sign-in provider. It is
// deliberately secret-free: no secret reference, token, or credential ever
// crosses this struct's JSON boundary.
type AuthProvider struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Status      string `json:"status"`
	Description string `json:"description,omitempty"`
}

// ProviderConfig is the internal configuration for a provider. It may carry a
// SecretRef (an opaque secret-store identifier, never a value) and other
// provider-specific settings that must not reach discovery.
type ProviderConfig struct {
	Name        string
	Label       string
	Status      string
	Family      contract.ProviderFamily
	Scopes      []string
	SecretRef   string // e.g. secret://auth/oidc/<id>/client-secret
	Description string
}

// ProviderRegistry is the normalized source of auth provider discovery. The
// providers map holds only safe discovery projections; configs holds the full
// internal configuration (including secret references) for provider flows.
type ProviderRegistry struct {
	providers map[string]AuthProvider
	configs   map[string]ProviderConfig
}

// NewProviderRegistry returns the default registry (password + magic-link
// supported, oidc planned). It is retained for backward compatibility.
func NewProviderRegistry() *ProviderRegistry {
	r, _ := NewProviderRegistryFromConfig(DefaultProviderConfigs())
	return r
}

// DefaultProviderConfigs returns the built-in provider configuration. OIDC is
// marked planned: it must not be advertised as supported until the AUTH-006
// acceptance suite passes.
func DefaultProviderConfigs() []ProviderConfig {
	return []ProviderConfig{
		{Name: "password", Label: "Email & Password", Status: string(contract.CapabilitySupported), Family: "password"},
		{Name: "magic_link", Label: "Magic Link", Status: string(contract.CapabilitySupported), Family: "magic_link", Description: "Passwordless email sign-in"},
		{Name: "oidc", Label: "OpenID Connect", Status: string(contract.CapabilityPlanned), Family: contract.ProviderFamilyOIDC, Description: "OIDC authorization-code + PKCE"},
	}
}

// NewProviderRegistryFromConfig builds a registry from typed configuration.
// Unknown provider status is rejected at load (fail closed): the registry
// returns an error rather than silently accepting an invalid capability.
func NewProviderRegistryFromConfig(configs []ProviderConfig) (*ProviderRegistry, error) {
	r := &ProviderRegistry{
		providers: make(map[string]AuthProvider),
		configs:   make(map[string]ProviderConfig),
	}
	for _, c := range configs {
		if !validStatus(c.Status) {
			return nil, fmt.Errorf("auth: provider %q has invalid status %q", c.Name, c.Status)
		}
		r.providers[c.Name] = AuthProvider{
			Name:        c.Name,
			Label:       c.Label,
			Status:      c.Status,
			Description: c.Description,
		}
		r.configs[c.Name] = c
	}
	return r, nil
}

// Register adds a discovery projection. It is retained for callers that manage
// providers directly (no secret refs are involved).
func (r *ProviderRegistry) Register(provider AuthProvider) {
	if r.providers == nil {
		r.providers = make(map[string]AuthProvider)
	}
	r.providers[provider.Name] = provider
}

// List returns the secret-free discovery projection, sorted by name.
func (r *ProviderRegistry) List() []AuthProvider {
	out := make([]AuthProvider, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Config returns the internal configuration for a provider, including its
// secret reference. It is intended for provider flows (AUTH-002+), not for
// discovery or logging; callers must never serialize SecretRef to a client.
func (r *ProviderRegistry) Config(name string) (ProviderConfig, bool) {
	c, ok := r.configs[name]
	return c, ok
}

// validStatus reports whether s is a member of the shared capability-status
// vocabulary from contract.CapabilityStatus.
func validStatus(s string) bool {
	switch contract.CapabilityStatus(s) {
	case contract.CapabilityPlanned, contract.CapabilityExperimental, contract.CapabilitySupported, contract.CapabilityRetired:
		return true
	default:
		return false
	}
}
