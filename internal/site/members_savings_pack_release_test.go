package site

import "testing"

func TestMembersModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "members-savings", erpModuleFiles(t, "members-savings"))
}

// Members is installed beside People, CRM and the other modules in niche
// templates, so it must compose with them.
func TestMembersComposesWithTheOtherModules(t *testing.T) {
	requireComposes(t, "composed-with-members", append([]string{"members-savings", "people", "crm"}, erpModules...)...)
}
