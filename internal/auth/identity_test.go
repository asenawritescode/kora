package auth

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestNewProviderRegistryFromConfigLoadsProviders(t *testing.T) {
	configs := []ProviderConfig{
		{Name: "oidc", Label: "OpenID Connect", Status: "planned", Family: contract.ProviderFamilyOIDC, SecretRef: "secret://auth/oidc/entra/client-secret"},
	}
	r, err := NewProviderRegistryFromConfig(configs)
	if err != nil {
		t.Fatalf("NewProviderRegistryFromConfig: %v", err)
	}
	list := r.List()
	if len(list) != 1 || list[0].Name != "oidc" {
		t.Fatalf("unexpected discovery projection: %+v", list)
	}
	cfg, ok := r.Config("oidc")
	if !ok || cfg.SecretRef != "secret://auth/oidc/entra/client-secret" {
		t.Fatalf("secret ref not retained in internal config: %+v", cfg)
	}
}

func TestNewProviderRegistryFromConfigRejectsInvalidStatus(t *testing.T) {
	_, err := NewProviderRegistryFromConfig([]ProviderConfig{
		{Name: "oidc", Status: "bogus"},
	})
	if err == nil {
		t.Fatalf("expected invalid status to be rejected")
	}
	if !strings.Contains(err.Error(), "invalid status") {
		t.Fatalf("error should mention invalid status: %v", err)
	}
}

func TestDiscoveryNeverLeaksSecretRef(t *testing.T) {
	r, err := NewProviderRegistryFromConfig([]ProviderConfig{
		{Name: "oidc", Label: "OIDC", Status: "planned", SecretRef: "secret://auth/oidc/x/secret"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	raw, _ := json.Marshal(r.List())
	if strings.Contains(string(raw), "secret://") || strings.Contains(string(raw), "client-secret") {
		t.Fatalf("discovery projection leaked secret reference: %s", raw)
	}
}

func TestDefaultRegistryKeepsBackwardCompatibleStatuses(t *testing.T) {
	r := NewProviderRegistry()
	if len(r.List()) < 3 {
		t.Fatalf("default registry too short")
	}
	for _, p := range r.List() {
		if !validStatus(p.Status) {
			t.Fatalf("provider %q has invalid status %q", p.Name, p.Status)
		}
	}
}
