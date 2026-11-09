package auth

import (
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestProviderRegistryDefaults(t *testing.T) {
	r := NewProviderRegistry()
	providers := r.List()
	if len(providers) < 3 {
		t.Fatalf("provider registry list too short: %+v", providers)
	}
	if providers[0].Name == "" {
		t.Fatal("expected named providers")
	}
	foundOIDC := false
	for _, p := range providers {
		if p.Name == "oidc" {
			foundOIDC = true
			if p.Status != "planned" {
				t.Fatalf("oidc status = %q, want planned", p.Status)
			}
		}
	}
	if !foundOIDC {
		t.Fatal("expected oidc provider to be registered")
	}
}

func TestProviderRegistryDiscoveryIsSecretFree(t *testing.T) {
	r, err := NewProviderRegistryFromConfig([]ProviderConfig{{
		Name:        "oidc:acme",
		Label:       "Acme OIDC",
		Status:      string(contract.CapabilityPlanned),
		Family:      contract.ProviderFamilyOIDC,
		SecretRef:   "secret://auth/oidc/acme/client-secret",
		Description: "OIDC provider for Acme",
	}})
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}

	providers := r.List()
	if len(providers) != 1 {
		t.Fatalf("registry list = %+v", providers)
	}
	if providers[0].Name != "oidc:acme" || providers[0].Status != string(contract.CapabilityPlanned) {
		t.Fatalf("unexpected discovery projection: %+v", providers[0])
	}
	if providers[0].Description != "OIDC provider for Acme" {
		t.Fatalf("unexpected description: %+v", providers[0])
	}
	if _, ok := r.Config("oidc:acme"); !ok {
		t.Fatal("expected internal provider config to remain available")
	}
	if cfg, ok := r.Config("oidc:acme"); !ok || cfg.SecretRef == "" {
		t.Fatalf("expected secret ref to stay internal: %+v", cfg)
	}
}

func TestProviderRegistryRejectsInvalidStatus(t *testing.T) {
	if _, err := NewProviderRegistryFromConfig([]ProviderConfig{{
		Name:   "broken",
		Label:  "Broken",
		Status: "unsupported-but-not-really",
	}}); err == nil {
		t.Fatal("expected invalid provider status to fail closed")
	}
}

func TestProviderRegistryAdvertisesOnlyEnabledConfig(t *testing.T) {
	r, err := NewProviderRegistryFromConfig([]ProviderConfig{
		{
			Name:   "password",
			Label:  "Email & Password",
			Status: string(contract.CapabilitySupported),
		},
		{
			Name:   "magic_link",
			Label:  "Magic Link",
			Status: string(contract.CapabilitySupported),
		},
	})
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	got := r.List()
	if len(got) != 2 {
		t.Fatalf("registry list = %+v, want only enabled config entries", got)
	}
	for _, p := range got {
		if p.Name == "oidc" {
			t.Fatal("disabled provider oidc should not be advertised")
		}
	}
}
