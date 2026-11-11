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
	packs := append([]string{"crm"}, erpModules...)
	var all []PackFile
	for _, pack := range packs {
		for _, file := range erpModuleFiles(t, pack) {
			if file.ContentType == "roles" || file.ContentType == "permissions" {
				continue
			}
			all = append(all, file)
		}
	}
	for _, kind := range []string{"roles", "permissions"} {
		merged := ""
		for _, pack := range packs {
			for _, file := range erpModuleFiles(t, pack) {
				if file.ContentType == kind {
					merged += file.Content
				}
			}
		}
		all = append(all, PackFile{Path: kind + ".yaml", ContentType: kind, Content: merged})
	}
	requireValidPack(t, "composed-with-crm", all)
	if _, _, err := ParseConfigPack("composed-with-crm", all); err != nil {
		t.Fatalf("CRM does not import beside the ERP modules: %v", err)
	}
}
