package kernel

import (
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func TestChildValueConvertsTableRowsOnlyForTableFields(t *testing.T) {
	dt := &doctype.DocType{Name: "Sale", Fields: []doctype.Field{
		{Fieldname: "status", Fieldtype: "Data"},
		{Fieldname: "items", Fieldtype: "Table", Options: "Sale Item"},
	}}
	rows := []any{map[string]any{"item": "tea"}}
	cases := []struct {
		name, field string
		in          any
		wantChild   bool
	}{
		{"table rows become children", "items", rows, true},
		{"table field with a non-list value stays as given", "items", "x", false},
		{"data field is untouched", "status", "Paid", false},
		{"unknown field is untouched", "missing", rows, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := childValue(dt, tc.field, tc.in)
			_, isChildren := got.([]*doctype.Document)
			if isChildren != tc.wantChild {
				t.Fatalf("childValue(%s) = %T, want children %v", tc.field, got, tc.wantChild)
			}
		})
	}
}

func TestAuthorizedStepsCheckAnUpsertAsItsCreateAndItsUpdate(t *testing.T) {
	upsert := CommandStep{As: "sale", ForEach: "", Upsert: &StepUpsert{
		Record: "Sale", Name: "$input.sale_number", InBlock: "$input.till_session", Values: map[string]string{"register": "$input.register"},
	}}
	plain := CommandStep{Update: &StepUpdate{Record: "Stock Balance", Name: "$input.item", Values: map[string]string{"quantity": "1"}}}
	got := authorizedSteps([]CommandStep{plain, upsert})
	if len(got) != 3 {
		t.Fatalf("authorizedSteps returned %d steps, want 3 (plain, upsert create, upsert update)", len(got))
	}
	if got[1].Create == nil || got[1].Create.Record != "Sale" || got[1].Create.Name != "$input.sale_number" || got[1].Update != nil {
		t.Fatalf("upsert create view = %+v", got[1])
	}
	if got[2].Update == nil || got[2].Update.Record != "Sale" || got[2].Update.Name != "$input.sale_number" || got[2].Create != nil {
		t.Fatalf("upsert update view = %+v", got[2])
	}
}

func TestUpsertStepParsesOnlyWithItsFields(t *testing.T) {
	const header = `name: save_sale
namespace: pos
version: 1
module_owner_id: pos.core
capability: pos.checkout
operation: save_sale
presentation:
  title: Save sale
permission: create
input:
  record: Sale
  extra:
    till_session: {type: string, required: true}
    sale_number: {type: string, required: true}
`
	cases := []struct {
		name, step string
		wantErr    bool
	}{
		{"complete", "transaction:\n  - upsert:\n      record: Sale\n      name: $input.sale_number\n      in_block: $input.till_session\n      values: {status: Held}\n", false},
		{"no name creates under the series", "transaction:\n  - upsert:\n      record: Sale\n      values: {status: Held}\n", false},
		{"no block", "transaction:\n  - upsert:\n      record: Sale\n      name: $input.sale_number\n      values: {status: Held}\n", true},
		{"no values", "transaction:\n  - upsert:\n      record: Sale\n      name: $input.sale_number\n      in_block: $input.till_session\n", true},
		{"upsert with create", "transaction:\n  - upsert:\n      record: Sale\n      name: $input.sale_number\n      in_block: $input.till_session\n      values: {status: Held}\n    create:\n      record: Sale\n      values: {status: Held}\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCommandResource([]byte(header + tc.step))
			if (err != nil) != tc.wantErr {
				t.Fatalf("parse error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}
