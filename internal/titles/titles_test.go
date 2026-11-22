package titles

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
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

func TestResolveKeepsRawValuesWhenALinkedTypeCannotBeRead(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	register := &doctype.DocType{Name: "Register", TitleField: "register_name", Fields: []doctype.Field{{Fieldname: "register_name", Fieldtype: "Data"}}}
	sale := &doctype.DocType{Name: "Sale", TitleField: "register", Fields: []doctype.Field{{Fieldname: "register", Fieldtype: "Link", Options: "Register"}}}
	mock.ExpectQuery("FROM `tabSale`").WillReturnRows(sqlmock.NewRows([]string{"name", "register"}).AddRow("SAL-1", "REGI-0001"))
	mock.ExpectQuery("FROM `tabRegister`").WillReturnError(errors.New("Error 1146: Table 'tabRegister' doesn't exist"))
	o := Options{DB: database, Dialect: db.Resolve("mysql"), Link: func(name string) *doctype.DocType {
		if name == "Register" {
			return register
		}
		return nil
	}}
	got, err := Resolve(context.Background(), o, sale, []string{"SAL-1"})
	if err != nil {
		t.Fatalf("a linked type that cannot be read must not fail the titles: %v", err)
	}
	if got["SAL-1"] != "REGI-0001" {
		t.Fatalf("title = %q, want the raw link value", got["SAL-1"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the linked type was not read: %v", err)
	}
}
