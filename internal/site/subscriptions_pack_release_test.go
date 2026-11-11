package site

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSubscriptionsModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "subscriptions", erpModuleFiles(t, "subscriptions"))
}

// A school, a property manager and a professional-services firm install
// Subscriptions beside the other modules, so it must compose with them: no
// clash with Selling's Customer and Payment, and no shared role names.
func TestSubscriptionsComposesWithTheOtherModules(t *testing.T) {
	requireComposes(t, "composed-with-subscriptions", append([]string{"subscriptions", "people", "crm"}, erpModules...)...)
}

// The pack ships without script hooks: tenant Engines run with scripts off.
func TestSubscriptionsPackShipsNoScripts(t *testing.T) {
	if _, err := os.Stat(filepath.Join("..", "..", "config", "subscriptions", "scripts")); !os.IsNotExist(err) {
		t.Fatalf("config/subscriptions must not have a scripts directory (stat error: %v)", err)
	}
}
