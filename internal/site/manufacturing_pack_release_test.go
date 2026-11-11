package site

import "testing"

func TestManufacturingModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "manufacturing", erpModuleFiles(t, "manufacturing"))
}

// Manufacturing is installed beside Selling, Stock, People, CRM and the others
// in niche templates, and links to Selling's Product, so it must compose with
// them.
func TestManufacturingComposesWithTheOtherModules(t *testing.T) {
	requireComposes(t, "composed-with-manufacturing", append([]string{"manufacturing", "people", "crm"}, erpModules...)...)
}
