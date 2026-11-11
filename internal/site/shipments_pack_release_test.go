package site

import "testing"

func TestShipmentsModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "shipments", erpModuleFiles(t, "shipments"))
}

// Shipments is installed beside Selling, Stock, CRM and the others in niche
// templates, so it must compose with them.
func TestShipmentsComposesWithTheOtherModules(t *testing.T) {
	requireComposes(t, "composed-with-shipments", append([]string{"shipments", "people", "crm"}, erpModules...)...)
}
