package engine

import (
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
)

func TestModuleVersionNewerThan(t *testing.T) {
	tests := []struct {
		name        string
		version     moduleVersion
		installed   moduleVersion
		wantNewer   bool
		wantInvalid bool
	}{
		{name: "major bump is newer", version: "2.0.0", installed: "1.0.0", wantNewer: true},
		{name: "minor bump compares numerically", version: "1.10.0", installed: "1.9.0", wantNewer: true},
		{name: "patch bump is newer", version: "1.0.1", installed: "1.0.0", wantNewer: true},
		{name: "short form pads with zeros", version: "2", installed: "1.9.9", wantNewer: true},
		{name: "equal is not newer", version: "1", installed: "1.0.0", wantNewer: false},
		{name: "downgrade is not newer", version: "1.0.0", installed: "2.0.0", wantNewer: false},
		{name: "prerelease is unparseable", version: "2.0.0-beta", installed: "1.0.0", wantInvalid: true},
		{name: "leading v is unparseable", version: "v2.0.0", installed: "1.0.0", wantInvalid: true},
		{name: "empty is unparseable", version: "", installed: "1.0.0", wantInvalid: true},
		{name: "empty component is unparseable", version: "1..2", installed: "1.0.0", wantInvalid: true},
		{name: "four components are unparseable", version: "1.0.0.0", installed: "1.0.0", wantInvalid: true},
		{name: "installed unparseable is refused", version: "2.0.0", installed: "1.0.0-rc1", wantInvalid: true},
		{name: "overflow is unparseable", version: "99999999999999999999.0.0", installed: "1.0.0", wantInvalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newer, err := tt.version.newerThan(tt.installed)
			if tt.wantInvalid {
				if err == nil {
					t.Fatalf("newerThan(%q, %q) accepted an unparseable version", tt.version, tt.installed)
				}
				return
			}
			if err != nil {
				t.Fatalf("newerThan(%q, %q) error = %v", tt.version, tt.installed, err)
			}
			if newer != tt.wantNewer {
				t.Fatalf("newerThan(%q, %q) = %v; want %v", tt.version, tt.installed, newer, tt.wantNewer)
			}
		})
	}
}

func replaceTestModule(version string, resources ...contract.OwnedResource) contract.ModuleDescriptor {
	return contract.ModuleDescriptor{ID: "pos.core", Version: version, Resources: resources}
}

func replaceTestResource(name string) contract.OwnedResource {
	return contract.OwnedResource{
		Ref:    contract.ResourceRef{Namespace: "tenant", Name: name, Version: 1},
		Access: contract.ModulePrivate, MutationMode: contract.MutationCommands, ScopeKind: contract.ScopeTenantShared,
	}
}

// posAuthority authorizes the pos package to replace modules it installed.
func posAuthority() packageAuthority {
	return packageAuthority{packageID: "pos", installedBy: map[contract.ModuleID]string{"pos.core": "pos"}}
}

func TestMergeManagementPackageSnapshotAuthorizedModuleReplacement(t *testing.T) {
	current := &doctype.ConfigSnapshot{
		Modules:  []contract.ModuleDescriptor{replaceTestModule("1.0.0", replaceTestResource("Sale"))},
		DocTypes: []*doctype.DocType{{Name: "Sale", ModuleOwnerID: "pos.core"}},
	}
	incoming := &doctype.ConfigSnapshot{
		Modules:  []contract.ModuleDescriptor{replaceTestModule("2.0.0", replaceTestResource("Sale"), replaceTestResource("Refund"))},
		DocTypes: []*doctype.DocType{{Name: "Sale", ModuleOwnerID: "pos.core", Module: "v2"}},
	}
	merged, err := mergeManagementPackageSnapshot(current, incoming, posAuthority())
	if err != nil {
		t.Fatalf("authorized newer replacement refused: %v", err)
	}
	if len(merged.Modules) != 1 || merged.Modules[0].Version != "2.0.0" || len(merged.Modules[0].Resources) != 2 {
		t.Fatalf("merged modules = %+v; want pos.core 2.0.0 with both resources", merged.Modules)
	}
}

func TestMergeManagementPackageSnapshotModuleReplacementRules(t *testing.T) {
	foreign := packageAuthority{packageID: "evil", installedBy: map[contract.ModuleID]string{"pos.core": "pos"}}
	tests := []struct {
		name      string
		current   contract.ModuleDescriptor
		incoming  contract.ModuleDescriptor
		authority packageAuthority
		wantErr   string
	}{
		{
			name:    "same version with changed content is refused",
			current: replaceTestModule("1.0.0"), incoming: replaceTestModule("1.0.0", replaceTestResource("Sale")),
			authority: posAuthority(), wantErr: "cannot change without an authorized version update",
		},
		{
			name:    "downgrade is refused",
			current: replaceTestModule("2.0.0"), incoming: replaceTestModule("1.0.0"),
			authority: posAuthority(), wantErr: "does not replace installed version",
		},
		{
			name:    "unparseable new version is refused",
			current: replaceTestModule("1.0.0"), incoming: replaceTestModule("2.0.0-beta"),
			authority: posAuthority(), wantErr: "is not a comparable version",
		},
		{
			name:    "replacement without authority is refused",
			current: replaceTestModule("1.0.0"), incoming: replaceTestModule("2.0.0"),
			authority: packageAuthority{}, wantErr: "requires explicit authorized replacement",
		},
		{
			name:    "package that did not install the module is refused",
			current: replaceTestModule("1.0.0"), incoming: replaceTestModule("2.0.0"),
			authority: foreign, wantErr: `installed by package "pos"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := mergeManagementPackageSnapshot(
				&doctype.ConfigSnapshot{Modules: []contract.ModuleDescriptor{tt.current}},
				&doctype.ConfigSnapshot{Modules: []contract.ModuleDescriptor{tt.incoming}},
				tt.authority,
			)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v; want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestMergeManagementPackageSnapshotRefusesForeignResourceOnReplacement(t *testing.T) {
	current := &doctype.ConfigSnapshot{Modules: []contract.ModuleDescriptor{
		replaceTestModule("1.0.0"),
		{ID: "finance.ar", Version: "1.0.0", Resources: []contract.OwnedResource{replaceTestResource("Invoice")}},
	}}
	incoming := &doctype.ConfigSnapshot{Modules: []contract.ModuleDescriptor{replaceTestModule("2.0.0", replaceTestResource("Invoice"))}}
	_, err := mergeManagementPackageSnapshot(current, incoming, posAuthority())
	if err == nil || !strings.Contains(err.Error(), `owned by module "finance.ar"`) {
		t.Fatalf("foreign resource error = %v; want owner conflict", err)
	}
}

func TestMergeManagementPackageSnapshotReplacementDropsRemovedOwnedDocTypes(t *testing.T) {
	current := &doctype.ConfigSnapshot{
		Modules: []contract.ModuleDescriptor{replaceTestModule("1.0.0", replaceTestResource("Sale"), replaceTestResource("Legacy"))},
		DocTypes: []*doctype.DocType{
			{Name: "Sale", ModuleOwnerID: "pos.core"},
			{Name: "Legacy", ModuleOwnerID: "pos.core"},
			{Name: "Customer"},
		},
	}
	incoming := &doctype.ConfigSnapshot{
		Modules:  []contract.ModuleDescriptor{replaceTestModule("2.0.0", replaceTestResource("Sale"))},
		DocTypes: []*doctype.DocType{{Name: "Sale", ModuleOwnerID: "pos.core"}},
	}
	merged, err := mergeManagementPackageSnapshot(current, incoming, posAuthority())
	if err != nil {
		t.Fatalf("replacement error = %v", err)
	}
	var names []string
	for _, dt := range merged.DocTypes {
		names = append(names, dt.Name)
	}
	if strings.Join(names, ",") != "Customer,Sale" {
		t.Fatalf("doctypes after replacement = %v; want the removed owned Legacy gone and tenant Customer kept", names)
	}
}

func TestCheckReplacedModulesQuiet(t *testing.T) {
	replaced := map[contract.ModuleID]bool{"pos.core": true, "pos.app": true}
	tests := []struct {
		name     string
		inFlight map[contract.ModuleID]int
		wantErr  string
	}{
		{name: "no operations", inFlight: map[contract.ModuleID]int{}},
		{name: "operations on an unchanged module are fine", inFlight: map[contract.ModuleID]int{"finance.ar": 3}},
		{name: "operation on a replaced module blocks", inFlight: map[contract.ModuleID]int{"pos.core": 1}, wantErr: `module "pos.core" has operations in progress; retry the upgrade when they finish`},
		{name: "first replaced module in ID order is named", inFlight: map[contract.ModuleID]int{"pos.core": 1, "pos.app": 2}, wantErr: `module "pos.app" has operations in progress; retry the upgrade when they finish`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkReplacedModulesQuiet(replaced, tt.inFlight)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("error = %v; want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("error = %v; want %q", err, tt.wantErr)
			}
			if _, ok := inFlightFailure(err); !ok {
				t.Fatalf("in-flight error is not classified as a refusal: %v", err)
			}
		})
	}
}
