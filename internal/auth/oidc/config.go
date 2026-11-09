package oidc

import (
	"errors"
	"strings"

	"github.com/asenawritescode/kora/contract"
)

// Config is the typed OIDC provider configuration (RFC §17.2). SecretRef is an
// opaque secret-store reference, never a credential value.
type Config struct {
	ID             string
	Issuer         string
	ClientID       string
	SecretRef      string // secret://auth/oidc/<id>/client-secret
	Scopes         []string
	AllowedDomains []string
	AllowedGroups  []string
	RoleMappingRef string
	JITProvision   bool
}

// Validate checks that a configuration is usable and fail-closed. JIT
// provisioning without a domain or group allow-list is a misconfiguration, not
// open provisioning.
func (c Config) Validate() error {
	if strings.TrimSpace(c.ID) == "" {
		return errors.New("oidc: provider id is required")
	}
	if strings.TrimSpace(c.Issuer) == "" {
		return errors.New("oidc: issuer is required")
	}
	if strings.TrimSpace(c.ClientID) == "" {
		return errors.New("oidc: client_id is required")
	}
	if c.SecretRef == "" {
		return errors.New("oidc: secret_ref is required")
	}
	if c.JITProvision && len(c.AllowedDomains) == 0 && len(c.AllowedGroups) == 0 {
		return errors.New("oidc: jit_provision requires allowed_domains or allowed_groups")
	}
	return nil
}

// Profile builds the public capability profile for discovery (AUTH-001).
func (c Config) Profile() contract.ProviderProfile {
	profile := contract.ProviderProfile{
		Family:       contract.ProviderFamilyOIDC,
		Status:       contract.CapabilityPlanned,
		Capabilities: []string{"authorization_code", "pkce"},
	}
	if c.JITProvision {
		profile.Capabilities = append(profile.Capabilities, "jit_provisioning")
	}
	profile.RequiredChecks = []string{"issuer", "audience", "expiry", "nonce", "jwks"}
	return profile
}
