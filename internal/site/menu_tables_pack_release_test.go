package site

import "testing"

func TestMenuModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "menu-tables", erpModuleFiles(t, "menu-tables"))
}

// Menu is installed beside People, CRM and the other modules in niche
// templates, so it must compose with them.
func TestMenuComposesWithTheOtherModules(t *testing.T) {
	requireComposes(t, "composed-with-menu", append([]string{"menu-tables", "people", "crm"}, erpModules...)...)
}
