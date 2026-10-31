package doctype

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var generatedSelectOption = regexp.MustCompile(`(?i)^(option|choice|value|status|item|field|type|stage)[ _#-]*[0-9]+$`)

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
			doctypes, err := ParseConfigTree(root)
			if err != nil {
				t.Fatalf("config pack does not parse: %v", err)
			}
			byName := make(map[string]*DocType, len(doctypes))
			for _, dt := range doctypes {
				byName[dt.Name] = dt
			}
			for _, source := range doctypes {
				for _, field := range source.Fields {
					if field.Fieldtype != "Link" {
						continue
					}
					target := byName[field.Options]
					// Links may target a DocType supplied by a separately selected
					// module (for example Helpdesk -> CRM Contact). Validate local
					// targets here; cross-package dependencies are covered by the
					// composed-package validation path.
					if target == nil {
						continue
					}
					titleName := strings.TrimSpace(target.TitleField)
					if titleName == "" || titleName == "name" {
						t.Errorf("%s.%s links to %s without a human-readable title_field", source.Name, field.Fieldname, target.Name)
						continue
					}
					title := target.GetField(titleName)
					if title == nil || title.Hidden || strings.EqualFold(title.Fieldtype, "Password") {
						t.Errorf("%s.%s target title %s.%s is missing, hidden, or sensitive", source.Name, field.Fieldname, target.Name, titleName)
						continue
					}
					if !title.SearchIndex {
						t.Errorf("%s.%s target title %s.%s lacks a database search index", source.Name, field.Fieldname, target.Name, titleName)
					}
					declared := false
					for _, candidate := range strings.Split(target.SearchFields, ",") {
						if strings.TrimSpace(candidate) == titleName {
							declared = true
							break
						}
					}
					if !declared {
						t.Errorf("%s.%s target title %s.%s is not in search_fields", source.Name, field.Fieldname, target.Name, titleName)
					}
				}
			}
			for _, doctype := range doctypes {
				for _, field := range doctype.Fields {
					if field.Fieldtype != "Select" {
						continue
					}
					var options []string
					for _, option := range strings.Split(field.Options, "\n") {
						if option = strings.TrimSpace(option); option != "" {
							options = append(options, option)
						}
					}
					if len(options) < 2 {
						t.Errorf("%s.%s Select must define at least two domain choices", doctype.Name, field.Fieldname)
					}
					for _, option := range options {
						if generatedSelectOption.MatchString(option) {
							t.Errorf("%s.%s has placeholder Select choice %q", doctype.Name, field.Fieldname, option)
						}
					}
				}
			}
			views, err := filepath.Glob(filepath.Join(root, "views", "*.yaml"))
			if err != nil || len(views) == 0 {
				t.Fatalf("pack has no workspace view")
			}
		})
	}
}
