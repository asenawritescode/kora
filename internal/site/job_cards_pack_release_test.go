package site

import "testing"

func TestWorkshopModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "job-cards", erpModuleFiles(t, "job-cards"))
}

// Workshop links to Product, so it is installed beside Selling, Stock, CRM,
// People and the others in niche templates, and must compose with them.
func TestWorkshopComposesWithTheOtherModules(t *testing.T) {
	requireComposes(t, "composed-with-workshop", append([]string{"job-cards", "people", "crm"}, erpModules...)...)
}
