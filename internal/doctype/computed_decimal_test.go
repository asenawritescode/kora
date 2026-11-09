package doctype

import (
	"encoding/json"
	"testing"
)

func TestDecimalComputedFields(t *testing.T) {
	child := func(amount any) *Document {
		row := NewDocument("Line")
		row.Set("amount", amount)
		return row
	}
	tests := []struct {
		name   string
		expr   string
		fields map[string]any
		lines  []*Document
		want   any
	}{
		{"no binary noise", "a + b", map[string]any{"a": 0.1, "b": 0.2}, nil, json.Number("0.3")},
		{"line total", "quantity * unit_price", map[string]any{"quantity": json.Number("3"), "unit_price": json.Number("19.99")}, nil, json.Number("59.97")},
		{"large totals keep cents", "a * b", map[string]any{"a": json.Number("1234567890.12"), "b": json.Number("3")}, nil, json.Number("3703703670.36")},
		{"sum of lines", "SUM(items.amount)", nil, []*Document{child(0.1), child(json.Number("0.1")), child("0.1"), child(nil)}, json.Number("0.3")},
		{"round half away from zero", "ROUND(subtotal * 0.16, 2)", map[string]any{"subtotal": json.Number("10.03125")}, nil, json.Number("1.61")},
		{"nested arithmetic", "subtotal - discount + tax", map[string]any{"subtotal": json.Number("100"), "discount": json.Number("2.5"), "tax": json.Number("15.6")}, nil, json.Number("113.1")},
		{"lisp syntax", "(* qty rate)", map[string]any{"qty": json.Number("2"), "rate": json.Number("0.35")}, nil, json.Number("0.7")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dt := &DocType{Name: "Invoice", Fields: []Field{
				{Fieldname: "items", Fieldtype: "Table", Options: "Line"},
				{Fieldname: "result", Fieldtype: "Currency", Computed: tt.expr},
			}}
			doc := NewDocument("Invoice")
			for k, v := range tt.fields {
				doc.Set(k, v)
			}
			if tt.lines != nil {
				doc.SetTable("items", tt.lines)
			}
			if err := ComputeFields(dt, doc); err != nil {
				t.Fatal(err)
			}
			if got := doc.Get("result"); got != tt.want {
				t.Fatalf("result = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestDecimalEvaluatorDefersUnsupportedExpressions(t *testing.T) {
	doc := NewDocument("Invoice")
	doc.Set("a", nil)
	for _, expr := range []string{`(if (> a 1) 2 3)`, `(+ a 1)`, `(datediff "x" "y")`, `(/ 1 0)`, `(round 1.5 x)`, `(sum "items")`} {
		if _, ok := evalDecimalSExpr(expr, doc); ok {
			t.Fatalf("%s should fall back to the Lisp sandbox", expr)
		}
	}
}
