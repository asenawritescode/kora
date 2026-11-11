package site

import "testing"

// requireComposes validates and imports packs together the way a niche template
// installs them: no clashing resource names, no clashing role definitions, no
// dangling links. Roles and permissions are merged as composition merges them.
func requireComposes(t *testing.T, name string, packs ...string) {
	t.Helper()
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
	requireValidPack(t, name, all)
	if _, _, err := ParseConfigPack(name, all); err != nil {
		t.Fatalf("%s does not import: %v", name, err)
	}
}
