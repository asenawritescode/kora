package doctype

import (
	"strings"
	"testing"
)

func stockTypes(effect *QuantityEffect, receiptEffect *QuantityEffect) []*DocType {
	product := &DocType{Name: "Product", Fields: []Field{
		{Fieldname: "product_name", Fieldtype: "Data"},
		{Fieldname: "stock_qty", Fieldtype: "Float"},
		{Fieldname: "label", Fieldtype: "Data"},
	}}
	other := &DocType{Name: "Supplier", Fields: []Field{{Fieldname: "name_text", Fieldtype: "Data"}}}
	adjustment := &DocType{Name: "Stock Adjustment", QuantityEffect: effect, Fields: []Field{
		{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "supplier", Fieldtype: "Link", Options: "Supplier"},
		{Fieldname: "delta", Fieldtype: "Float"},
		{Fieldname: "note", Fieldtype: "Data"},
	}}
	line := &DocType{Name: "Receipt Line", IsChildTable: true, Fields: []Field{
		{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "quantity", Fieldtype: "Float"},
		{Fieldname: "memo", Fieldtype: "Data"},
	}}
	receipt := &DocType{Name: "Receipt", QuantityEffect: receiptEffect, Fields: []Field{
		{Fieldname: "items", Fieldtype: "Table", Options: "Receipt Line"},
		{Fieldname: "note", Fieldtype: "Data"},
	}}
	return []*DocType{product, other, adjustment, line, receipt}
}

func goodHeaderEffect() *QuantityEffect {
	return &QuantityEffect{TargetDocType: "Product", TargetQuantityField: "stock_qty", SourceRecordField: "product", DeltaField: "delta"}
}

func goodLinesEffect() *QuantityEffect {
	return &QuantityEffect{TargetDocType: "Product", TargetQuantityField: "stock_qty", LinesField: "items", SourceRecordField: "product", DeltaField: "quantity", Direction: "increase"}
}

func effectProblems(t *testing.T, header, lines *QuantityEffect) string {
	t.Helper()
	var all []string
	for _, problem := range ValidateAll(stockTypes(header, lines)) {
		all = append(all, problem.Code+": "+problem.Message)
	}
	return strings.Join(all, "\n")
}

func TestQuantityEffectOnTheRecordNeedsNoView(t *testing.T) {
	if got := effectProblems(t, goodHeaderEffect(), goodLinesEffect()); got != "" {
		t.Fatalf("a record-level effect with no view was rejected:\n%s", got)
	}
}

func TestQuantityEffectIsCheckedAgainstTheRecordTypes(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(h, l *QuantityEffect)
		want   string
	}{
		{"target record type missing", func(h, l *QuantityEffect) { h.TargetDocType = "Nope" }, "target_doctype"},
		{"target field missing", func(h, l *QuantityEffect) { h.TargetQuantityField = "absent" }, "target_quantity_field"},
		{"target field is not a number", func(h, l *QuantityEffect) { h.TargetQuantityField = "label" }, "target_quantity_field"},
		{"source field missing", func(h, l *QuantityEffect) { h.SourceRecordField = "absent" }, "source_record_field"},
		{"source field links to something else", func(h, l *QuantityEffect) { h.SourceRecordField = "supplier" }, "source_record_field"},
		{"amount field is not a number", func(h, l *QuantityEffect) { h.DeltaField = "note" }, "delta_field"},
		{"amount field missing", func(h, l *QuantityEffect) { h.DeltaField = "absent" }, "delta_field"},
		{"lines field is not a table", func(h, l *QuantityEffect) { l.LinesField = "note" }, "lines_field"},
		{"lines field missing", func(h, l *QuantityEffect) { l.LinesField = "absent" }, "lines_field"},
		{"line product field missing", func(h, l *QuantityEffect) { l.SourceRecordField = "absent" }, "source_record_field"},
		{"line amount is not a number", func(h, l *QuantityEffect) { l.DeltaField = "memo" }, "delta_field"},
		{"unknown direction", func(h, l *QuantityEffect) { l.Direction = "sideways" }, "direction"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, l := goodHeaderEffect(), goodLinesEffect()
			tc.mutate(h, l)
			got := effectProblems(t, h, l)
			if !strings.Contains(got, "InvalidQuantityEffect") || !strings.Contains(got, tc.want) {
				t.Fatalf("expected an InvalidQuantityEffect problem mentioning %q, got:\n%s", tc.want, got)
			}
		})
	}
}

func TestQuantityEffectOnASubmittableRecordIsRefused(t *testing.T) {
	types := stockTypes(goodHeaderEffect(), goodLinesEffect())
	for _, dt := range types {
		if dt.Name == "Receipt" {
			dt.IsSubmittable = true
		}
	}
	var got []string
	for _, problem := range ValidateAll(types) {
		got = append(got, problem.Code+": "+problem.Message)
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "InvalidQuantityEffect") || !strings.Contains(joined, "submittable") {
		t.Fatalf("a quantity effect on a submittable record must be refused, got:\n%s", joined)
	}
}

func TestQuantityEffectKeepsWorkingWithAViewBinding(t *testing.T) {
	h := goodHeaderEffect()
	h.View, h.Component, h.Action = "Stock adjustment action", "adjustment", "apply"
	if got := effectProblems(t, h, goodLinesEffect()); got != "" {
		t.Fatalf("an effect that also names a view was rejected:\n%s", got)
	}
}

func TestQuantityEffectParsesFromYAML(t *testing.T) {
	dt, err := ParseYAML([]byte(`
name: Receipt
module: Stock
fields:
  - fieldname: items
    fieldtype: Table
    options: Receipt Line
quantity_effect:
  target_doctype: Product
  target_quantity_field: stock_qty
  lines_field: items
  source_record_field: product
  delta_field: quantity
  direction: decrease
`), "inline")
	if err != nil {
		t.Fatal(err)
	}
	q := dt.QuantityEffect
	if q == nil || q.LinesField != "items" || q.Direction != "decrease" || q.View != "" {
		t.Fatalf("effect did not parse as expected: %#v", q)
	}
}

func TestQuantityEffectReasonDirectionsAreChecked(t *testing.T) {
	build := func(directions map[string]string, lines string, reasonType string) []CrossFileError {
		product := &DocType{Name: "Product", Fields: []Field{{Fieldname: "stock_qty", Fieldtype: "Float"}}}
		fields := []Field{
			{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
			{Fieldname: "delta", Fieldtype: "Float"},
			{Fieldname: "reason", Fieldtype: reasonType, Options: "Damaged\nFound"},
		}
		adj := &DocType{Name: "Adj", Fields: fields, QuantityEffect: &QuantityEffect{
			TargetDocType: "Product", TargetQuantityField: "stock_qty", SourceRecordField: "product", DeltaField: "delta",
			ReasonField: "reason", ReasonDirections: directions, LinesField: lines,
		}}
		return ValidateAll([]*DocType{product, adj})
	}
	if errs := build(map[string]string{"Damaged": "decrease", "Found": "increase"}, "", "Select"); len(errs) != 0 {
		t.Fatalf("valid config rejected: %v", errs)
	}
	for name, errs := range map[string][]CrossFileError{
		"unknown direction":    build(map[string]string{"Damaged": "sideways"}, "", "Select"),
		"reason not an option": build(map[string]string{"Stolen": "decrease"}, "", "Select"),
		"reason is not Select": build(map[string]string{"Damaged": "decrease"}, "", "Data"),
	} {
		if len(errs) == 0 {
			t.Errorf("%s was accepted", name)
		}
	}
}
