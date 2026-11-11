package site

import "testing"

func TestPeopleModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "people", erpModuleFiles(t, "people"))
}

// People is installed beside Selling, Stock, CRM and the others in niche
// templates, so it must compose with them.
func TestPeopleComposesWithTheOtherModules(t *testing.T) {
	requireComposes(t, "composed-with-people", append([]string{"people", "crm"}, erpModules...)...)
}
