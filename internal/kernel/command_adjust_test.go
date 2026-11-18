package kernel

import (
	"math/big"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

const adjustYAML = `name: library.return_books
version: 1
input:
  record: Shelf
transaction:
  - for_each: $input.books
    update:
      record: Shelf
      where:
        book: $item.book
      adjust:
        copies: { increase: $item.copies }
`

func TestAdjustParsesAndRejectsInvalidDefinitions(t *testing.T) {
	if _, err := ParseCommandResource([]byte(adjustYAML)); err != nil {
		t.Fatal(err)
	}
	for name, yaml := range map[string]string{
		"both directions": strings.Replace(adjustYAML, "{ increase: $item.copies }", "{ increase: $item.copies, decrease: $item.copies }", 1),
		"bad min":         strings.Replace(adjustYAML, "{ increase: $item.copies }", "{ increase: $item.copies, min: lots }", 1),
		"name and where":  strings.Replace(adjustYAML, "      where:", "      name: $item.book\n      where:", 1),
	} {
		if _, err := ParseCommandResource([]byte(yaml)); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestApplyAdjustmentsIsExactAndHonoursFloor(t *testing.T) {
	dt := &doctype.DocType{Name: "Shelf", Fields: []doctype.Field{{Fieldname: "copies", Fieldtype: "Int"}, {Fieldname: "weight", Fieldtype: "Float"}}}
	old := doctype.NewDocument("Shelf")
	old.Set("copies", int64(3))
	old.Set("weight", 0.1)
	doc := doctype.NewDocument("Shelf")
	env := refEnv{input: map[string]any{"n": float64(2), "w": "0.2"}}
	if err := applyAdjustments(dt, old, doc, map[string]FieldAdjust{"copies": {Decrease: "$input.n", Min: "0"}, "weight": {Increase: "$input.w"}}, stepEnv{env: env}); err != nil {
		t.Fatal(err)
	}
	if doc.Get("copies") != int64(1) || doc.Get("weight") != "0.3" {
		t.Fatalf("copies=%v weight=%v", doc.Get("copies"), doc.Get("weight"))
	}
	if err := applyAdjustments(dt, old, doc, map[string]FieldAdjust{"copies": {Decrease: "$input.n", Min: "2"}}, stepEnv{env: env}); err == nil || err.Type != "CONFLICT" {
		t.Fatalf("floor not enforced: %v", err)
	}
	if err := applyAdjustments(dt, old, doc, map[string]FieldAdjust{"copies": {Increase: "$input.w"}}, stepEnv{env: env}); err == nil {
		t.Fatal("fractional change to an Int field was accepted")
	}
}

func TestFieldAdjustValidateAcceptsOnlyWellFormedForms(t *testing.T) {
	cases := []struct {
		name   string
		change FieldAdjust
		ok     bool
	}{
		{"plain increase", FieldAdjust{Increase: "$input.n"}, true},
		{"plain decrease with floor", FieldAdjust{Decrease: "$input.n", Min: "0"}, true},
		{"plain needs one side", FieldAdjust{}, false},
		{"plain both sides", FieldAdjust{Increase: "$input.n", Decrease: "$input.n"}, false},
		{"signed", FieldAdjust{By: "$input.n", SignFrom: "$input.d", IncreaseWhen: "In", DecreaseWhen: "Out"}, true},
		{"signed with floor", FieldAdjust{By: "$input.n", SignFrom: "$input.d", IncreaseWhen: "In", DecreaseWhen: "Out", Min: "0"}, true},
		{"signed missing sign_from", FieldAdjust{By: "$input.n", IncreaseWhen: "In", DecreaseWhen: "Out"}, false},
		{"signed one-sided increase", FieldAdjust{By: "$input.n", SignFrom: "$input.d", IncreaseWhen: "Out"}, true},
		{"signed one-sided decrease", FieldAdjust{By: "$input.n", SignFrom: "$input.d", DecreaseWhen: "Out"}, true},
		{"signed no direction", FieldAdjust{By: "$input.n", SignFrom: "$input.d"}, false},
		{"signed missing by", FieldAdjust{SignFrom: "$input.d", IncreaseWhen: "In", DecreaseWhen: "Out"}, false},
		{"signed with increase too", FieldAdjust{Increase: "$input.n", By: "$input.n", SignFrom: "$input.d", IncreaseWhen: "In", DecreaseWhen: "Out"}, false},
		{"signed same direction values", FieldAdjust{By: "$input.n", SignFrom: "$input.d", IncreaseWhen: "In", DecreaseWhen: "In"}, false},
		{"signed bad floor", FieldAdjust{By: "$input.n", SignFrom: "$input.d", IncreaseWhen: "In", DecreaseWhen: "Out", Min: "lots"}, false},
	}
	for _, tc := range cases {
		if err := tc.change.validate(); (err == nil) != tc.ok {
			t.Errorf("%s: validate() = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestApplySignedAdjustmentsFollowsDirectionValue(t *testing.T) {
	dt := &doctype.DocType{Name: "Till Session", Fields: []doctype.Field{{Fieldname: "expected_cash", Fieldtype: "Currency"}}}
	signed := map[string]FieldAdjust{"expected_cash": {By: "$input.amount", SignFrom: "$input.direction", IncreaseWhen: "In", DecreaseWhen: "Out", Min: "0"}}
	cases := []struct {
		name, direction string
		start           float64
		want            string
		wantErr         string
	}{
		{"in adds", "In", 100, "112.5", ""},
		{"out subtracts", "Out", 100, "87.5", ""},
		{"out to exact floor", "Out", 12.5, "0", ""},
		{"out below floor refused", "Out", 10, "", "CONFLICT"},
		{"unknown direction refused", "Sideways", 100, "", "VALIDATION_FAILED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := doctype.NewDocument("Till Session")
			old.Set("expected_cash", tc.start)
			doc := doctype.NewDocument("Till Session")
			env := refEnv{input: map[string]any{"amount": "12.5", "direction": tc.direction}}
			err := applyAdjustments(dt, old, doc, signed, stepEnv{env: env})
			if tc.wantErr != "" {
				if err == nil || string(err.Type) != tc.wantErr {
					t.Fatalf("err = %v, want %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := doc.Get("expected_cash"); got != tc.want {
				t.Fatalf("expected_cash = %v, want %s", got, tc.want)
			}
		})
	}
}

func TestApplyOneSidedSignedAdjustmentCountsOnlyItsDirection(t *testing.T) {
	dt := &doctype.DocType{Name: "Till Session", Fields: []doctype.Field{{Fieldname: "payout_count", Fieldtype: "Int"}}}
	counts := map[string]FieldAdjust{"payout_count": {By: "1", SignFrom: "$input.direction", IncreaseWhen: "Out"}}
	cases := []struct {
		name, direction string
		start, want     int64
	}{
		{"out counts", "Out", 3, 4},
		{"in leaves count", "In", 3, 3},
		{"unknown leaves count", "Sideways", 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := doctype.NewDocument("Till Session")
			old.Set("payout_count", tc.start)
			doc := doctype.NewDocument("Till Session")
			env := refEnv{input: map[string]any{"direction": tc.direction}}
			if err := applyAdjustments(dt, old, doc, counts, stepEnv{env: env}); err != nil {
				t.Fatal(err)
			}
			got := doc.Get("payout_count")
			if tc.start == tc.want && got != nil {
				t.Fatalf("payout_count written = %v, want untouched", got)
			}
			if tc.start != tc.want && got != tc.want {
				t.Fatalf("payout_count = %v, want %d", got, tc.want)
			}
		})
	}
}

func TestMoveExactAndStoredNumberKeepDecimalsExact(t *testing.T) {
	tests := []struct {
		name      string
		current   string
		delta     string
		increase  bool
		fieldtype string
		want      any
	}{
		{"add decimals", "0.1", "0.2", true, "Float", "0.3"},
		{"subtract to whole", "1.5", "0.5", false, "Currency", "1"},
		{"int stays integer", "7", "2", false, "Int", int64(5)},
		{"float trims zeros", "2", "0.25", true, "Float", "2.25"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			current, _ := new(big.Rat).SetString(tc.current)
			delta, _ := new(big.Rat).SetString(tc.delta)
			if got := storedNumber(tc.fieldtype, moveExact(current, delta, tc.increase)); got != tc.want {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}
