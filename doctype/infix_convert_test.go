package doctype

import (
	"testing"
)

// TestInfixToSExpr verifies that common infix computed field expressions
// are correctly converted to their s-expression equivalents.
func TestInfixToSExpr(t *testing.T) {
	tests := []struct {
		infix string
		want  string
	}{
		{"qty * unit_price", "(* qty unit_price)"},
		{"qty * price - discount", "(- (* qty price) discount)"},
		{"total / 3", "(/ total 3)"},
		{"a + b + c", "(+ (+ a b) c)"},
		{"SUM(items.amount)", `(sum "items" "amount")`},
		{"COUNT(attendees)", `(count "attendees")`},
		{"ROUND(price * qty, 2)", "(round (* price qty) 2)"},
		{"DATEDIFF(start_date, end_date)", "(datediff start_date end_date)"},
		{"DATEDIFF(today(), due_date)", "(datediff (today) due_date)"},
	}

	for _, tt := range tests {
		got := InfixToSExpr(tt.infix)
		if got != tt.want {
			t.Errorf("InfixToSExpr(%q) = %q, want %q", tt.infix, got, tt.want)
		}
	}
}

// TestInfixToSExpr_RoundTrip verifies that converted expressions evaluate
// to the same result as the original infix through the legacy evaluator.
func TestInfixToSExpr_RoundTrip(t *testing.T) {

	inputs := map[string]any{"qty": 5.0, "unit_price": 12.5, "price": 10.0, "discount": 2.0}
	childTables := map[string][]map[string]any{
		"items": {{"amount": 10.0}, {"amount": 20.0}},
	}

	tests := []string{
		"qty * unit_price",
		"SUM(items.amount)",
	}

	for _, infix := range tests {
		sExpr := InfixToSExpr(infix)
		if sExpr == infix {
			t.Errorf("converter did not convert %q", infix)
			continue
		}

		// Evaluate through legacy.
		legacyDT := &DocType{Name: "Test", Fields: []Field{{Fieldname: "r", Fieldtype: "Float", Computed: infix}}}
		legacyDoc := NewDocument("Test")
		for k, v := range inputs {
			legacyDoc.Set(k, v)
		}
		for tn, rows := range childTables {
			children := make([]*Document, len(rows))
			for j, row := range rows {
				c := NewDocument("Child")
				for ck, cv := range row {
					c.Set(ck, cv)
				}
				children[j] = c
			}
			legacyDoc.SetTable(tn, children)
		}
		if err := ComputeFields(legacyDT, legacyDoc); err != nil {
			t.Fatalf("legacy ComputeFields(%q): %v", infix, err)
		}
		legacyVal := legacyDoc.GetFloat("r")

		// Evaluate through Lisp sandbox.
		s := NewLispSandbox()
		defer s.Close()
		lispVal, err := s.Eval(sExpr, inputs, childTables)
		if err != nil {
			t.Fatalf("Lisp Eval(%q) -> %q: %v", infix, sExpr, err)
		}
		if legacyVal != lispVal {
			t.Errorf("%q: legacy=%v, lisp=%v", infix, legacyVal, lispVal)
		}
	}
}
