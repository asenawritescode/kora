package configstore

import (
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
)

// A pending pack hook binds a DocType that an installed pack defines when the
// installed package record lists it, even without a module ownership row.
func TestCheckPendingScriptBindingsUsesRecordedDocTypes(t *testing.T) {
	hook := func(requires ...string) *doctype.ScriptSnapshot {
		return &doctype.ScriptSnapshot{
			Name: "pos__Stock_Entry_before_save", OwnerPack: "pos", DocType: "Stock Entry", Event: "before_save",
			ScriptType: "doc_event", PendingBinding: true, OwnerRequires: requires,
		}
	}
	tests := []struct {
		name      string
		packages  []contract.InstalledPackage
		script    *doctype.ScriptSnapshot
		wantError bool
	}{
		{
			name:     "requires an installed pack whose record lists the DocType",
			packages: []contract.InstalledPackage{{PackageID: "inventory", ReleaseID: "r1", DocTypes: []string{"Stock Entry"}}},
			script:   hook("inventory"),
		},
		{
			name:      "installed pack without the DocType in its record refuses the hook",
			packages:  []contract.InstalledPackage{{PackageID: "inventory", ReleaseID: "r1", DocTypes: []string{"Item"}}},
			script:    hook("inventory"),
			wantError: true,
		},
		{
			name:      "pack without requires refuses the hook",
			packages:  []contract.InstalledPackage{{PackageID: "inventory", ReleaseID: "r1", DocTypes: []string{"Stock Entry"}}},
			script:    hook(),
			wantError: true,
		},
		{
			name:      "package installed before records existed keeps index-only binding",
			packages:  []contract.InstalledPackage{{PackageID: "inventory", ReleaseID: "r1"}},
			script:    hook("inventory"),
			wantError: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckPendingScriptBindings(contract.ModuleSnapshot{Packages: tc.packages}, []*doctype.ScriptSnapshot{tc.script})
			if (err != nil) != tc.wantError {
				t.Fatalf("CheckPendingScriptBindings error = %v, want error %v", err, tc.wantError)
			}
		})
	}
}
