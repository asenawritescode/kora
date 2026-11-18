package kernel

import (
	"slices"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestRequiredInputs(t *testing.T) {
	const head = "name: shop.do\nversion: 1\ninput: {record: Thing}\n"
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{"guard and step values: only selectors count", head + `
require:
  - lock: {record: Register, name: $input.register}
  - exists: {record: Register, where: {name: $input.register, location: $input.location}}
  - not_exists: {record: Shift, where: {status: Open, register: $input.shift_register}}
transaction:
  - create: {record: Thing, values: {note: $input.note, register: $input.register}}
`, []string{"location", "register", "shift_register"}},
		{"update selectors, adjust, for_each, where and compute", head + `
compute:
  total: {sum: $input.items, expr: "amount"}
transaction:
  - for_each: $input.lines
    where: {kind: $input.kind}
    update: {record: Stock, where: {item: $item.item, location: $input.location}, adjust: {quantity: {decrease: $input.qty}}}
  - update: {record: Till, name: $input.till, values: {note: $input.note}}
`, []string{"items", "kind", "lines", "location", "qty", "till"}},
		{"guard waived for roles is not required", head + `
require:
  - exists: {record: Till, where: {name: $input.till, cashier: $actor}}
    unless_role: [Owner]
transaction:
  - create: {record: Thing, values: {till: $input.till}}
`, nil},
		{"nested path names its top-level key", head + `
require:
  - exists: {record: Till, where: {name: $input.till.name}}
transaction:
  - create: {record: Thing, values: {a: $input.a}}
`, []string{"till"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := ParseCommandResource([]byte(tt.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if got := cmd.RequiredInputs(); !slices.Equal(got, tt.want) {
				t.Fatalf("RequiredInputs = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMissingInputNamesTheField(t *testing.T) {
	cmd, err := ParseCommandResource([]byte(`name: pos.open
version: 1
input: {record: Till Session}
require:
  - exists: {record: Register, where: {name: $input.register, location: $input.location}}
transaction:
  - create: {record: Till Session, values: {register: $input.register}}
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name  string
		input map[string]any
		field string
	}{
		{"nothing sent", map[string]any{}, "location"},
		{"location missing", map[string]any{"register": "R-1"}, "location"},
		{"register missing", map[string]any{"location": "L-1"}, "register"},
		{"complete", map[string]any{"register": "R-1", "location": "L-1"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := cmd.inputError(tt.input)
			if tt.field == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err == nil || err.Type != contract.CodeValidationFailed || err.Field != tt.field || err.Message != "input "+tt.field+" is required" {
				t.Fatalf("err = %+v, want input %s is required on field %s", err, tt.field, tt.field)
			}
		})
	}
}

// TestInputEnumRefusesOtherValues: an enum input accepts only its listed values,
// and the refusal names the input. Absent inputs are the required check's job.
func TestInputEnumRefusesOtherValues(t *testing.T) {
	cmd, err := ParseCommandResource([]byte(`name: pos.record_pay_in
version: 1
module_owner_id: pos.core
capability: pos.drawer
operation: record_pay_in
input_schema: {id: pos.record_pay_in.input, version: 1}
output_schema: {id: pos.record_pay_in.output, version: 1}
input: {record: Cash Movement, enum: {reason: [Other, Owner added cash]}}
transaction:
  - create: {record: Cash Movement, values: {reason: $input.reason}}
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name  string
		input map[string]any
		field string
	}{
		{"listed value", map[string]any{"reason": "Other"}, ""},
		{"value of the other list", map[string]any{"reason": "Transport"}, "reason"},
		{"non-string value", map[string]any{"reason": 1}, "reason"},
		{"null value", map[string]any{"reason": nil}, "reason"},
		{"absent input is not this check", map[string]any{}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := cmd.inputError(tt.input)
			if tt.field == "" {
				if err != nil {
					t.Fatalf("err = %+v", err)
				}
				return
			}
			if err == nil || err.Type != contract.CodeValidationFailed || err.Field != tt.field || err.Message != "input reason must be one of: Other, Owner added cash" {
				t.Fatalf("err = %+v, want the enum refusal on %s", err, tt.field)
			}
		})
	}
}

// TestInputEnumNeedsModuleOwnedCommand: an enum is published and enforced only
// by module-owned commands, and must list values.
func TestInputEnumNeedsModuleOwnedCommand(t *testing.T) {
	for _, tt := range []struct{ name, yaml, wantErr string }{
		{"not module owned", "input: {record: Thing, enum: {reason: [A]}}", "requires a module-owned command"},
		{"empty list", "module_owner_id: pos.core\ncapability: pos.drawer\ninput: {record: Thing, enum: {reason: []}}", "lists no values"},
		{"extra input not a string", "module_owner_id: pos.core\ncapability: pos.drawer\ninput: {record: Thing, extra: {n: {type: integer}}, enum: {n: [1]}}", "must be a string input"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseCommandResource([]byte("name: shop.do\nversion: 1\n" + tt.yaml + "\ntransaction:\n  - create: {record: Thing, values: {a: $input.a}}\n"))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
