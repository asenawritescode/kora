package site

import (
	"testing"

	"github.com/asenawritescode/kora/contract"
)

// The root pack.yaml requires must match the marketplace manifest's requires: the
// manifest is what the Engine records and enforces.
func TestPackRootRequiresMustMatchManifest(t *testing.T) {
	ref := func(id string) contract.ManifestPackRef { return contract.ManifestPackRef{PackageID: id, Version: "1"} }
	packYAML := func(requires string) []PackFile {
		return []PackFile{{Path: "pack.yaml", ContentType: "pack", Content: "name: pos\nversion: \"1\"\nrequires:\n" + requires}}
	}
	tests := []struct {
		name      string
		manifest  []contract.ManifestPackRef
		files     []PackFile
		wantError bool
	}{
		{
			name:     "same single requirement",
			manifest: []contract.ManifestPackRef{ref("inventory")},
			files:    packYAML("  - package_id: inventory\n    version: \"1\"\n"),
		},
		{
			name:     "same requirements in another order",
			manifest: []contract.ManifestPackRef{ref("b"), ref("a")},
			files:    packYAML("  - package_id: a\n    version: \"1\"\n  - package_id: b\n    version: \"1\"\n"),
		},
		{
			name:      "pack.yaml requires a pack the manifest does not",
			manifest:  nil,
			files:     packYAML("  - package_id: inventory\n    version: \"1\"\n"),
			wantError: true,
		},
		{
			name:      "manifest requires a pack pack.yaml omits",
			manifest:  []contract.ManifestPackRef{ref("inventory")},
			files:     packYAML(""),
			wantError: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckPackRootRequires(tc.manifest, tc.files)
			if (err != nil) != tc.wantError {
				t.Fatalf("CheckPackRootRequires error = %v, want error %v", err, tc.wantError)
			}
		})
	}
}
