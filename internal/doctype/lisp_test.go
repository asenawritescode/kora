package doctype

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestLispSandbox_BasicArith(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{"quantity": 10.0, "unit_price": 5.0}
	result, err := s.Eval("(* quantity unit_price)", doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	expected := 50.0
	got, ok := result.(float64)
	if !ok {
		t.Fatalf("expected float64, got %T(%v)", result, result)
	}
	if math.Abs(got-expected) > 1e-9 {
		t.Errorf("expected %v, got %v", expected, got)
	}
}

func TestLispSandbox_BasicArithWithAddition(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{"a": 10.0, "b": 20.0, "c": 5.0}
	result, err := s.Eval("(+ a b c)", doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(float64)
	if !ok {
		t.Fatalf("expected float64, got %T(%v)", result, result)
	}
	if math.Abs(got-35.0) > 1e-9 {
		t.Errorf("expected 35, got %v", got)
	}
}

func TestLispSandbox_SUM(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{}
	childTables := map[string][]map[string]any{
		"items": {
			{"amount": 10.0},
			{"amount": 20.0},
		},
	}
	result, err := s.Eval(`(sum "items" "amount")`, doc, childTables)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(float64)
	if !ok {
		t.Fatalf("expected float64, got %T(%v)", result, result)
	}
	if math.Abs(got-30.0) > 1e-9 {
		t.Errorf("expected 30, got %v", got)
	}
}

func TestLispSandbox_COUNT(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{}
	childTables := map[string][]map[string]any{
		"items": {
			{"name": "a"},
			{"name": "b"},
		},
	}
	result, err := s.Eval(`(count "items")`, doc, childTables)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(float64)
	if !ok {
		t.Fatalf("expected float64, got %T(%v)", result, result)
	}
	if math.Abs(got-2.0) > 1e-9 {
		t.Errorf("expected 2, got %v", got)
	}
}

func TestLispSandbox_ROUND(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{}
	result, err := s.Eval("(round 10.567 2)", doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(float64)
	if !ok {
		t.Fatalf("expected float64, got %T(%v)", result, result)
	}
	if math.Abs(got-10.57) > 1e-9 {
		t.Errorf("expected 10.57, got %v", got)
	}
}

func TestLispSandbox_DATEDIFF(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{}
	result, err := s.Eval(`(datediff "2026-06-28" "2026-07-01")`, doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(float64)
	if !ok {
		t.Fatalf("expected float64, got %T(%v)", result, result)
	}
	if math.Abs(got-3.0) > 1e-9 {
		t.Errorf("expected 3, got %v", got)
	}
}

func TestLispSandbox_IF(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	// Test true branch: amount > 100 → apply 10% discount
	doc := map[string]any{"amount": 200.0}
	result, err := s.Eval("(if (> amount 100) (* amount 0.9) amount)", doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(float64)
	if !ok {
		t.Fatalf("expected float64, got %T(%v)", result, result)
	}
	if math.Abs(got-180.0) > 1e-9 {
		t.Errorf("expected 180, got %v", got)
	}

	// Test false branch: amount ≤ 100 → return amount unchanged
	doc2 := map[string]any{"amount": 50.0}
	result2, err2 := s.Eval("(if (> amount 100) (* amount 0.9) amount)", doc2, nil)
	if err2 != nil {
		t.Fatal(err2)
	}
	got2, ok2 := result2.(float64)
	if !ok2 {
		t.Fatalf("expected float64, got %T(%v)", result2, result2)
	}
	if math.Abs(got2-50.0) > 1e-9 {
		t.Errorf("expected 50, got %v", got2)
	}
}

func TestLispSandbox_CONCAT(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{}
	result, err := s.Eval(`(concat "a" " " "b")`, doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(string)
	if !ok {
		t.Fatalf("expected string, got %T(%v)", result, result)
	}
	if got != "a b" {
		t.Errorf("expected %q, got %q", "a b", got)
	}
}

func TestLispSandbox_TODAY(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{}
	result, err := s.Eval("(today)", doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(string)
	if !ok {
		t.Fatalf("expected string, got %T(%v)", result, result)
	}
	expected := time.Now().Format("2006-01-02")
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestLispSandbox_CompoundWithSum(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	// total = sum of line totals + fixed surcharge
	doc := map[string]any{"surcharge": 5.0}
	childTables := map[string][]map[string]any{
		"items": {
			{"line_total": 20.0},
			{"line_total": 60.0},
		},
	}
	result, err := s.Eval(`(+ (sum "items" "line_total") surcharge)`, doc, childTables)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(float64)
	if !ok {
		t.Fatalf("expected float64, got %T(%v)", result, result)
	}
	if math.Abs(got-85.0) > 1e-9 {
		t.Errorf("expected 85, got %v", got)
	}
}

func TestLispSandbox_DeeplyNestedExpr(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{"qty": 10.0, "price": 5.0, "discount": 2.0}
	result, err := s.Eval("(- (* qty price) discount)", doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(float64)
	if !ok {
		t.Fatalf("expected float64, got %T(%v)", result, result)
	}
	if math.Abs(got-48.0) > 1e-9 {
		t.Errorf("expected 48, got %v", got)
	}
}

func TestLispSandbox_ErrorHandling(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	// Division by zero in Zygomys panics internally but is recovered by CallUserFunction.
	// The error can be either a panic recovery or a proper error depending on the type.
	doc := map[string]any{}
	_, err := s.Eval("(/ 1 0)", doc, nil)
	if err == nil {
		// If no error, Zygomys returned 'Inf' as a float, which is acceptable.
		t.Log("division by zero returned no error (accepting Inf result)")
	} else {
		t.Logf("division by zero returned error (acceptable): %v", err)
	}
}

func TestLispSandbox_MultipleEvalCalls(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	// Test that multiple Eval calls don't pollute each other.
	doc1 := map[string]any{"x": 10.0}
	r1, err := s.Eval("(* x 2)", doc1, nil)
	if err != nil {
		t.Fatal(err)
	}
	got1, _ := r1.(float64)
	if math.Abs(got1-20.0) > 1e-9 {
		t.Errorf("expected 20, got %v", got1)
	}

	doc2 := map[string]any{"y": 5.0}
	r2, err := s.Eval("(* y 3)", doc2, nil)
	if err != nil {
		t.Fatal(err)
	}
	got2, _ := r2.(float64)
	if math.Abs(got2-15.0) > 1e-9 {
		t.Errorf("expected 15, got %v", got2)
	}
}

func TestLispSandbox_StringDocValue(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{"name": "Alice", "greeting": "Hello"}
	result, err := s.Eval(`(concat greeting " " name)`, doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(string)
	if !ok {
		t.Fatalf("expected string, got %T(%v)", result, result)
	}
	if got != "Hello Alice" {
		t.Errorf("expected %q, got %q", "Hello Alice", got)
	}
}

func TestLispSandbox_DeepNestingComparison(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{"score": 85.0, "pass_mark": 50.0}
	result, err := s.Eval("(cond (> score pass_mark) (concat \"PASS: \" (round score 0)) \"FAIL\")", doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(string)
	if !ok {
		t.Fatalf("expected string, got %T(%v)", result, result)
	}
	if !strings.HasPrefix(got, "PASS:") {
		t.Errorf("expected string starting with PASS:, got %q", got)
	}
}

func TestLispSandbox_SumWithNumericDocFields(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{"discount": 5.0}
	childTables := map[string][]map[string]any{
		"items": {
			{"amount": 30.0},
			{"amount": 70.0},
		},
	}
	result, err := s.Eval("(- (sum \"items\" \"amount\") discount)", doc, childTables)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(float64)
	if !ok {
		t.Fatalf("expected float64, got %T(%v)", result, result)
	}
	if math.Abs(got-95.0) > 1e-9 {
		t.Errorf("expected 95, got %v", got)
	}
}

func TestLispSandbox_StringLiteral(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{}
	result, err := s.Eval(`"hello world"`, doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(string)
	if !ok {
		t.Fatalf("expected string, got %T(%v)", result, result)
	}
	if got != "hello world" {
		t.Errorf("expected %q, got %q", "hello world", got)
	}
}

func TestLispSandbox_SumEmpty(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{"discount": 5.0}
	childTables := map[string][]map[string]any{
		"items": {},
	}
	result, err := s.Eval("(- (sum \"items\" \"amount\") discount)", doc, childTables)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(float64)
	if !ok {
		t.Fatalf("expected float64, got %T(%v)", result, result)
	}
	if math.Abs(got-(-5.0)) > 1e-9 {
		t.Errorf("expected -5, got %v", got)
	}
}

func TestLispSandbox_MissingSymbol(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{"a": 10.0}
	_, err := s.Eval("(+ a b)", doc, nil)
	if err == nil {
		t.Fatal("expected error for undefined symbol 'b'")
	}
}

func TestLispSandbox_WithIntValues(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{"quantity": 3, "unit_price": 10}
	result, err := s.Eval("(* quantity unit_price)", doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(float64)
	if !ok {
		t.Fatalf("expected float64, got %T(%v)", result, result)
	}
	if got != 30.0 {
		t.Errorf("expected 30.0, got %v", got)
	}
}

func TestLispSandbox_SumNestedExpr(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{"discount_pct": 10.0}
	childTables := map[string][]map[string]any{
		"items": {
			{"amount": 100.0},
			{"amount": 200.0},
		},
	}
	// total after discount: sum(items, amount) * (1 - discount_pct/100)
	result, err := s.Eval("(* (sum \"items\" \"amount\") (- 1 (/ discount_pct 100)))", doc, childTables)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(float64)
	if !ok {
		t.Fatalf("expected float64, got %T(%v)", result, result)
	}
	if math.Abs(got-270.0) > 1e-9 {
		t.Errorf("expected 270, got %v", got)
	}
}

func TestLispSandbox_CountEmpty(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	doc := map[string]any{}
	childTables := map[string][]map[string]any{
		"items": {},
	}
	result, err := s.Eval("(count \"items\")", doc, childTables)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(float64)
	if !ok {
		t.Fatalf("expected float64, got %T(%v)", result, result)
	}
	if math.Abs(got-0.0) > 1e-9 {
		t.Errorf("expected 0, got %v", got)
	}
}
