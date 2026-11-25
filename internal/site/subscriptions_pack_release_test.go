package site

import "testing"

func TestSubscriptionsModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "subscriptions", packFiles(t, "subscriptions"))
}

// A school, a property manager and a professional-services firm install
// Subscriptions beside the other modules, so it must compose with them: no
// clash with Selling's Customer and Payment, and no shared role names.
func TestSubscriptionsComposesWithTheOtherModules(t *testing.T) {
	requireComposes(t, "composed-with-subscriptions", append([]string{"subscriptions", "people", "crm"}, baseModules...)...)
}
