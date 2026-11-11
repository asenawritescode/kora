package site

import "testing"

func TestProjectsModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "projects", erpModuleFiles(t, "projects"))
}

// Projects is installed beside Selling, Stock, CRM and the others in niche
// templates, so it must compose with them.
func TestProjectsComposesWithTheOtherModules(t *testing.T) {
	requireComposes(t, "composed-with-projects", append([]string{"projects", "people", "crm"}, erpModules...)...)
}
