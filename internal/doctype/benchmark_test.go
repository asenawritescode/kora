package doctype

import (
	"testing"
)

// BenchmarkLispSandbox_SimpleArith benchmarks the Lisp evaluator for simple arithmetic.
func BenchmarkLispSandbox_SimpleArith(b *testing.B) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{"quantity": 10.0, "unit_price": 5.0}
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := s.Eval("(* quantity unit_price)", doc, nil)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLispSandbox_WithSum benchmarks the Lisp evaluator with child table aggregation.
func BenchmarkLispSandbox_WithSum(b *testing.B) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{"discount": 5.0}
	children := map[string][]map[string]any{
		"items": {
			{"amount": 10.0},
			{"amount": 20.0},
			{"amount": 30.0},
			{"amount": 40.0},
			{"amount": 50.0},
		},
	}
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := s.Eval("(- (sum \"items\" \"amount\") discount)", doc, children)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLispSandbox_Nested benchmarks the Lisp evaluator with a complex nested expression.
func BenchmarkLispSandbox_Nested(b *testing.B) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{"a": 10.0, "b": 5.0, "c": 2.0, "d": 3.0}
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := s.Eval("(round (* (+ a b) (/ c d)) 2)", doc, nil)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkExistingEval benchmarks the existing expr-lang based evaluator for comparison.
func BenchmarkExistingEval(b *testing.B) {
	dt := &DocType{
		Name: "BenchType",
		Fields: []Field{
			{Fieldname: "result", Fieldtype: "Float", Computed: "quantity * unit_price"},
		},
	}

	doc := &Document{
		Fields: map[string]any{
			"quantity":   10.0,
			"unit_price": 5.0,
		},
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		err := ComputeFields(dt, doc)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkExistingEvalWithSum benchmarks the existing evaluator with child table sum.
func BenchmarkExistingEvalWithSum(b *testing.B) {
	dt := &DocType{
		Name: "BenchType",
		Fields: []Field{
			{Fieldname: "total", Fieldtype: "Float", Computed: "SUM(items.amount) - discount"},
		},
	}

	doc := &Document{
		Fields: map[string]any{
			"discount": 5.0,
			"items": []*Document{
				{Fields: map[string]any{"amount": 10.0}},
				{Fields: map[string]any{"amount": 20.0}},
				{Fields: map[string]any{"amount": 30.0}},
				{Fields: map[string]any{"amount": 40.0}},
				{Fields: map[string]any{"amount": 50.0}},
			},
		},
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		err := ComputeFields(dt, doc)
		if err != nil {
			b.Fatal(err)
		}
	}
}
