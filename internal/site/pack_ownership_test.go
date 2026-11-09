package site

import (
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
)

func ownedDT(name, owner, mode, access string, children ...string) *doctype.DocType {
	dt := &doctype.DocType{Name: name, ModuleOwnerID: owner, ModuleOwnershipMode: mode, ModuleAccess: contract.ModuleAccess(access)}
	for _, c := range children {
		dt.Fields = append(dt.Fields, doctype.Field{Fieldname: strings.ToLower(c), Fieldtype: "Table", Options: c})
	}
	return dt
}

func TestPackOwnership(t *testing.T) {
	mf := contract.PackManifest{Name: "shop", Version: "1"}
	tests := []struct {
		name    string
		dts     []*doctype.DocType
		wantErr string
		want    map[string]string // doctype -> mode/access
	}{
		{"commands_only and crud, children inherit",
			[]*doctype.DocType{ownedDT("Sale", "shop.core", "commands_only", "published", "Sale Item"), ownedDT("Item", "shop.core", "crud", ""),
				{Name: "Sale Item", IsChildTable: true}, {Name: "Stock Balance"}},
			"", map[string]string{"Sale": "commands_only/published", "Sale Item": "commands_only/published", "Item": "crud/private"}},
		{"wrong owner", []*doctype.DocType{ownedDT("Sale", "other.core", "crud", "")}, "pack.module_owner_mismatch", nil},
		{"invalid mode", []*doctype.DocType{ownedDT("Sale", "shop.core", "write", "")}, "pack.module_ownership_mode_invalid", nil},
		{"invalid access", []*doctype.DocType{ownedDT("Sale", "shop.core", "crud", "open")}, "pack.module_access_invalid", nil},
		{"mixed child owners",
			[]*doctype.DocType{ownedDT("Sale", "shop.core", "commands_only", "", "Line"), ownedDT("Quote", "shop.core", "crud", "", "Line"), {Name: "Line", IsChildTable: true}},
			"pack.child_owner_conflict", nil},
		{"child own keys differ from parent",
			[]*doctype.DocType{ownedDT("Sale", "shop.core", "commands_only", "", "Line"), ownedDT("Line", "shop.core", "crud", "")},
			"pack.child_owner_conflict", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snap := &doctype.ConfigSnapshot{DocTypes: tc.dts}
			mods, err := GeneratePackModules(mf, snap)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, r := range mods[0].Resources {
				got[r.Ref.Name] = string(r.MutationMode) + "/" + string(r.Access)
				if r.ScopeKind != contract.ScopeTenantShared {
					t.Fatalf("scope %v", r.ScopeKind)
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("resources %v want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("%s = %q want %q", k, got[k], v)
				}
			}
			applyPackOwnership(snap, mods)
			if c := tc.dts[len(tc.dts)-2]; c.Name == "Sale Item" && c.ModuleOwnershipMode != "commands_only" {
				t.Fatalf("child not applied: %+v", c)
			}
		})
	}
}
