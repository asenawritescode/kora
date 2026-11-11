package site

import "testing"

func TestFleetModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "fleet", erpModuleFiles(t, "fleet"))
}

// Fleet is installed beside People, CRM and the other modules in niche
// templates, so it must compose with them.
func TestFleetComposesWithTheOtherModules(t *testing.T) {
	requireComposes(t, "composed-with-fleet", append([]string{"fleet", "people", "crm"}, erpModules...)...)
}
