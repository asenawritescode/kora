package kernel

import (
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
	if err := applyAdjustments(dt, old, doc, map[string]FieldAdjust{"copies": {Decrease: "$input.n", Min: "0"}, "weight": {Increase: "$input.w"}}, env, 0); err != nil {
		t.Fatal(err)
	}
	if doc.Get("copies") != int64(1) || doc.Get("weight") != "0.3" {
		t.Fatalf("copies=%v weight=%v", doc.Get("copies"), doc.Get("weight"))
	}
	if err := applyAdjustments(dt, old, doc, map[string]FieldAdjust{"copies": {Decrease: "$input.n", Min: "2"}}, env, 0); err == nil || err.Type != "CONFLICT" {
		t.Fatalf("floor not enforced: %v", err)
	}
	if err := applyAdjustments(dt, old, doc, map[string]FieldAdjust{"copies": {Increase: "$input.w"}}, env, 0); err == nil {
		t.Fatal("fractional change to an Int field was accepted")
	}
}
