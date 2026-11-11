package site

import "testing"

func TestGrantsModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "grants-donors", erpModuleFiles(t, "grants-donors"))
}

// Grants is installed beside People, CRM and the other modules in niche
// templates, so it must compose with them.
func TestGrantsComposesWithTheOtherModules(t *testing.T) {
	requireComposes(t, "composed-with-grants", append([]string{"grants-donors", "people", "crm"}, erpModules...)...)
}
