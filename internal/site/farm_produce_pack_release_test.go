package site

import "testing"

func TestProduceModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "farm-produce", erpModuleFiles(t, "farm-produce"))
}

// Produce links to Product, so it is installed beside Selling and the other
// modules in niche templates and must compose with them.
func TestProduceComposesWithTheOtherModules(t *testing.T) {
	requireComposes(t, "composed-with-produce", append([]string{"farm-produce", "people", "crm"}, erpModules...)...)
}
