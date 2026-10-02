package doctype

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPRDTemplatePacksHaveCoreArtifacts(t *testing.T) {
	packs := []string{
		"small-business", "accounting", "invoicing", "expense", "budgeting", "kora-mvp",
		"crm", "marketing", "helpdesk", "customer-success", "inventory",
		"purchasing", "retail-pos", "ecommerce", "wholesale", "subscriptions",
		"projectmgmt", "professional-services", "fieldwork", "maintenance",
		"construction", "manufacturing", "quality", "fleet", "logistics", "hr",
		"recruitment", "payroll", "school", "clinic", "pharmacy", "membership",
		"hotel", "restaurant", "propertymgmt", "agriculture", "ngo", "sacco",
		"events", "documents", "internal-requests",
	}

	for _, pack := range packs {
		t.Run(pack, func(t *testing.T) {
			root := filepath.Join("../config", "v0", pack)
			if pack == "retail-pos" || pack == "kora-mvp" {
				root = filepath.Join("../config", pack)
			}
			for _, required := range []string{"roles.yaml", "permissions.yaml", "fixtures/demo.yaml"} {
				if _, err := os.Stat(filepath.Join(root, required)); err != nil {
					t.Fatalf("missing required artifact %s: %v", required, err)
				}
			}
			if _, err := ParseConfigTree(root); err != nil {
				t.Fatalf("config pack does not parse: %v", err)
			}
			views, err := filepath.Glob(filepath.Join(root, "views", "*.yaml"))
			if err != nil || len(views) == 0 {
				t.Fatalf("pack has no workspace view")
			}
		})
	}
}
