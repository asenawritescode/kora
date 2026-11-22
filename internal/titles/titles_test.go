package titles

import (
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
)

func TestReadable(t *testing.T) {
	published := contract.ModulePublished
	cases := []struct {
		name string
		dt   *doctype.DocType
		want bool
	}{
		{"nil record type", nil, false},
		{"published with a title field", &doctype.DocType{Name: "Register", TitleField: "register_name", Fields: []doctype.Field{{Fieldname: "register_name", Fieldtype: "Data"}}}, true},
		{"title field missing from the record type", &doctype.DocType{Name: "Register", TitleField: "gone"}, false},
		{"hidden title field", &doctype.DocType{Name: "Register", TitleField: "secret", Fields: []doctype.Field{{Fieldname: "secret", Fieldtype: "Data", Hidden: true}}}, false},
		{"password title field", &doctype.DocType{Name: "Login", TitleField: "pin", Fields: []doctype.Field{{Fieldname: "pin", Fieldtype: "Password"}}}, false},
		{"private module resource", &doctype.DocType{Name: "Ledger", TitleField: "name", ModuleOwnerID: "acct.core", ModuleAccess: contract.ModulePrivate}, false},
		{"module resource published", &doctype.DocType{Name: "Shift", TitleField: "name", ModuleOwnerID: "pos.core", ModuleAccess: published}, true},
		{"entity-owned record type", &doctype.DocType{Name: "Customer", TitleField: "name", BusinessScopePolicy: contract.ScopeEntityOwned}, false},
		{"name title with no field", &doctype.DocType{Name: "Tag", TitleField: "name"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Readable(tc.dt); got != tc.want {
				t.Fatalf("Readable = %v, want %v", got, tc.want)
			}
		})
	}
}
