package site

import "testing"

func TestPropertiesModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "properties", erpModuleFiles(t, "properties"))
}

// Properties is installed beside People, CRM and the ERP modules in niche
// templates, so it must compose with them.
func TestPropertiesComposesWithTheOtherModules(t *testing.T) {
	requireComposes(t, "composed-with-properties", append([]string{"properties", "people", "crm"}, erpModules...)...)
}
