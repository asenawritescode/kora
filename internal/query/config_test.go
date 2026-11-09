package query

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func testReg() *doctype.Registry {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Purchase Request", Fields: []doctype.Field{
		{Fieldname: "title", Fieldtype: "Data"},
		{Fieldname: "status", Fieldtype: "Select"},
		{Fieldname: "secret", Fieldtype: "Password"},
		{Fieldname: "lines", Fieldtype: "Table"},
	}})
	return reg
}

const valid = `
name: open_requests
module_owner_id: purchasing
capability: purchasing.requests
operation: open_requests
source: Purchase Request
permission: read
scope: legal_entity
input:
  search: {type: string, max_length: 80}
filters:
  - {field: status, op: eq, value: Draft}
  - any:
      - {field: title, op: contains, value: $input.search}
    optional: true
fields: [name, title, status, modified]
sort: [{field: modified, dir: desc}]
`

func TestParse(t *testing.T) {
	tests := []struct {
		name, yaml, wantErr string
	}{
		{"valid", valid, ""},
		{"unknown key", valid + "bogus: 1\n", "bogus"},
		{"missing field on doctype", strings.Replace(valid, "title, status", "nope, status", 1), `"nope" not on`},
		{"password field", strings.Replace(valid, "title, status", "secret, status", 1), "unselectable"},
		{"table field", strings.Replace(valid, "title, status", "lines, status", 1), "unselectable"},
		{"bad operator", strings.Replace(valid, "op: eq", "op: like", 1), "unknown operator"},
		{"bad scope", strings.Replace(valid, "legal_entity", "global", 1), "scope"},
		{"undeclared input", strings.Replace(valid, "$input.search", "$input.zzz", 1), "undeclared input"},
		{"unknown source", strings.Replace(valid, "source: Purchase Request", "source: Nope", 1), "not found"},
		{"field ref ok", strings.Replace(valid, "value: Draft", "value: $field.title", 1), ""},
		{"field ref unknown", strings.Replace(valid, "value: Draft", "value: $field.nope", 1), `"nope" not on`},
		{"field ref contains", strings.Replace(valid, "op: eq, value: Draft", "op: contains, value: $field.title", 1), "not allowed"},
		{"field ref optional", strings.Replace(valid, "value: Draft}", "value: $field.title, optional: true}", 1), "cannot be optional"},
		{"limit too big", valid + "limit: 500\n", "limit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Parse([]byte(tc.yaml), testReg())
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if d.Limit != 50 || d.CatalogRef() != "purchasing~purchasing.requests~open_requests" {
					t.Fatalf("defaults wrong: %+v", d)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), ErrInvalidDefinition) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	if ds, err := LoadDir(filepath.Join(dir, "none"), testReg()); err != nil || ds != nil {
		t.Fatalf("missing dir: %v %v", ds, err)
	}
	_ = os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(valid), 0o644)
	if ds, err := LoadDir(dir, testReg()); err != nil || len(ds) != 1 {
		t.Fatalf("load: %v %v", ds, err)
	}
	_ = os.WriteFile(filepath.Join(dir, "b.yaml"), []byte(valid), 0o644)
	if _, err := LoadDir(dir, testReg()); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate, got %v", err)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	reg := testReg()
	d, err := Parse([]byte(valid), reg)
	if err != nil {
		t.Fatal(err)
	}
	raws, err := Encode([]*Definition{d})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(raws, reg)
	if err != nil || len(got) != 1 {
		t.Fatalf("decode: %v %d", err, len(got))
	}
	if !reflect.DeepEqual(got[0], d) {
		t.Fatalf("round trip mismatch:\n%+v\n%+v", got[0], d)
	}
	if _, err := Decode(append(raws, raws...), reg); err == nil || !strings.Contains(err.Error(), ErrInvalidDefinition) {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestDecodeInvalid(t *testing.T) {
	_, err := Decode([]json.RawMessage{[]byte(`{"name":"x"}`)}, testReg())
	if err == nil || !strings.Contains(err.Error(), ErrInvalidDefinition) {
		t.Fatalf("got %v", err)
	}
}

func TestParseInputDefault(t *testing.T) {
	base := "name: q\nmodule_owner_id: m\ncapability: c\noperation: o\nsource: Purchase Request\npermission: read\nscope: tenant\nfields: [name]\ninput:\n  since: {type: %s, default: %s}\n"
	tests := []struct{ name, typ, def, wantErr string }{
		{"today", "string", "$today", ""},
		{"literal int", "int", "5", ""},
		{"bool", "bool", "true", ""},
		{"unknown reference", "string", "$input.other", "not a supported reference"},
		{"type mismatch", "int", "abc", "does not match type"},
		{"string for bool", "bool", "yes", "does not match type"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(fmt.Sprintf(base, tc.typ, tc.def)), testReg())
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
