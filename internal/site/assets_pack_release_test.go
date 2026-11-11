package site

import "testing"

func TestAssetsModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "assets", erpModuleFiles(t, "assets"))
}

// Assets is installed beside People, CRM and the other modules in niche
// templates, so it must compose with them.
func TestAssetsComposesWithTheOtherModules(t *testing.T) {
	requireComposes(t, "composed-with-assets", append([]string{"assets", "people", "crm"}, erpModules...)...)
}
