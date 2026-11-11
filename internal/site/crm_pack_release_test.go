package site

import "testing"

// The CRM module is published on its own and alongside the ERP modules, so it
// must pass the marketplace validation alone and import cleanly when composed
// with them (no clashing resource names, roles or dangling links).
func TestCRMModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "crm", erpModuleFiles(t, "crm"))
}

func TestCRMModuleImportsOnItsOwn(t *testing.T) {
	if _, _, err := ParseConfigPack("crm", erpModuleFiles(t, "crm")); err != nil {
		t.Fatalf("the CRM module does not import: %v", err)
	}
}

// A niche template installs CRM beside the ERP modules, so the whole set must
// validate and import as one.
func TestCRMComposesWithTheERPModules(t *testing.T) {
	requireComposes(t, "composed-with-crm", append([]string{"crm"}, erpModules...)...)
}
