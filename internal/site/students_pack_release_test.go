package site

import "testing"

func TestStudentsModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "students", erpModuleFiles(t, "students"))
}

// Students is installed beside People, CRM and the other modules in niche
// templates, so it must compose with them.
func TestStudentsComposesWithTheOtherModules(t *testing.T) {
	requireComposes(t, "composed-with-students", append([]string{"students", "people", "crm"}, erpModules...)...)
}
