package site

import "testing"

func TestDispensaryModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "batch-expiry", erpModuleFiles(t, "batch-expiry"))
}

// Dispensary is installed beside Selling (its medicines are Products), People,
// CRM and the other modules in niche templates, so it must compose with them.
func TestDispensaryComposesWithTheOtherModules(t *testing.T) {
	requireComposes(t, "composed-with-batch-expiry", append([]string{"batch-expiry", "people", "crm"}, erpModules...)...)
}
