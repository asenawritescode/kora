package site

import "testing"

func TestDocumentsModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "documents", erpModuleFiles(t, "documents"))
}

// Documents is installed beside People, CRM and the other modules in niche
// templates, so it must compose with them.
func TestDocumentsComposesWithTheOtherModules(t *testing.T) {
	requireComposes(t, "composed-with-documents", append([]string{"documents", "people", "crm"}, erpModules...)...)
}
