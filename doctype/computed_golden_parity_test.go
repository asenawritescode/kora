package doctype

import (
	"math"
	"testing"
)

// parityCase pairs a legacy infix expression with its s-expression equivalent.
type parityCase struct {
	infixExpr   string
	lispExpr    string
	inputs      map[string]any
	childTables map[string][]map[string]any
}

// TestGoldenZygomysParity ensures the Zygomys sandbox produces identical results
// to the legacy expr-lang evaluator for the same computations.
func TestGoldenZygomysParity(t *testing.T) {

	cases := []parityCase{
		// Simple arithmetic.
		{infixExpr: "qty * unit_price", lispExpr: "(* qty unit_price)",
			inputs: map[string]any{"qty": 5.0, "unit_price": 12.5}},
		{infixExpr: "qty * price - discount", lispExpr: "(- (* qty price) discount)",
			inputs: map[string]any{"qty": 10.0, "price": 5.0, "discount": 2.0}},
		{infixExpr: "total / 3", lispExpr: "(/ total 3)",
			inputs: map[string]any{"total": 100.0}},
		{infixExpr: "a + b + c", lispExpr: "(+ a b c)",
			inputs: map[string]any{"a": 10.0, "b": 20.0, "c": 5.0}},

		// SUM aggregation.
		{infixExpr: "SUM(items.amount)", lispExpr: `(sum "items" "amount")`,
			inputs:      map[string]any{},
			childTables: map[string][]map[string]any{"items": {{"amount": 10.0}, {"amount": 20.0}}}},
		{infixExpr: "SUM(items.line_total)", lispExpr: `(sum "items" "line_total")`,
			inputs:      map[string]any{},
			childTables: map[string][]map[string]any{"items": {{"line_total": 20.0}, {"line_total": 60.0}}}},

		// COUNT aggregation.
		{infixExpr: "COUNT(attendees)", lispExpr: `(count "attendees")`,
			inputs:      map[string]any{},
			childTables: map[string][]map[string]any{"attendees": {{"name": "a"}, {"name": "b"}}}},
	}

	for i, c := range cases {
		// --- Legacy path ---
		dt := &DocType{
			Name:   "TestType",
			Fields: []Field{{Fieldname: "result", Fieldtype: "Float", Computed: c.infixExpr}},
		}

		doc := NewDocument("TestType")
		for k, v := range c.inputs {
			doc.Set(k, v)
		}
		for tableName, rows := range c.childTables {
			children := make([]*Document, len(rows))
			for j, row := range rows {
				child := NewDocument("ChildType")
				for ck, cv := range row {
					child.Set(ck, cv)
				}
				children[j] = child
			}
			doc.SetTable(tableName, children)
		}

		err := ComputeFields(dt, doc)
		if err != nil {
			t.Errorf("case %d: legacy ComputeFields(%q) error: %v", i, c.infixExpr, err)
			continue
		}
		legacyResult := doc.GetFloat("result")

		// --- Zygomys path ---
		s := NewLispSandbox()
		lispResult, err := s.Eval(c.lispExpr, c.inputs, c.childTables)
		s.Close()
		if err != nil {
			t.Errorf("case %d: Zygomys Eval(%q) error: %v", i, c.lispExpr, err)
			continue
		}

		lispFloat, ok := lispResult.(float64)
		if !ok {
			t.Errorf("case %d: Zygomys result is %T(%v), expected float64", i, lispResult, lispResult)
			continue
		}

		// --- Compare ---
		if math.Abs(legacyResult-lispFloat) > 1e-9 {
			t.Errorf("case %d: DIVERGENCE — legacy=%v, zygomys=%v (expr: %q vs %q)",
				i, legacyResult, lispFloat, c.infixExpr, c.lispExpr)
		}
	}
}

// TestGoldenZygomysParity_DateFunctions verifies date function parity.
func TestGoldenZygomysParity_DateFunctions(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	// ROUND: both should round identically.
	legacyDT := &DocType{
		Name:   "TestType",
		Fields: []Field{{Fieldname: "result", Fieldtype: "Float", Computed: "ROUND(price * qty, 2)"}},
	}
	legacyDoc := NewDocument("TestType")
	legacyDoc.Set("price", 10.567)
	legacyDoc.Set("qty", 3.0)
	if err := ComputeFields(legacyDT, legacyDoc); err != nil {
		t.Fatal(err)
	}
	legacyRounded := legacyDoc.GetFloat("result")

	lispRounded, err := s.Eval("(round (* price qty) 2)", map[string]any{"price": 10.567, "qty": 3.0}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(legacyRounded-lispRounded.(float64)) > 1e-9 {
		t.Errorf("ROUND divergence: legacy=%v, zygomys=%v", legacyRounded, lispRounded)
	}

	// DATEDIFF: both should compute same day difference.
	legacyDT2 := &DocType{
		Name:   "TestType",
		Fields: []Field{{Fieldname: "result", Fieldtype: "Float", Computed: "DATEDIFF(start_date, end_date)"}},
	}
	legacyDoc2 := NewDocument("TestType")
	legacyDoc2.Set("start_date", "2026-06-29")
	legacyDoc2.Set("end_date", "2026-07-01")
	if err := ComputeFields(legacyDT2, legacyDoc2); err != nil {
		t.Fatal(err)
	}
	legacyDiff := legacyDoc2.GetFloat("result")

	lispDiff, err := s.Eval(`(datediff "2026-06-29" "2026-07-01")`, map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(legacyDiff-lispDiff.(float64)) > 1e-9 {
		t.Errorf("DATEDIFF divergence: legacy=%v, zygomys=%v", legacyDiff, lispDiff)
	}
}

