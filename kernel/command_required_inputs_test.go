package kernel

import (
	"slices"
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
			err := cmd.missingInput(tt.input)
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
