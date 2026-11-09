package expression

import (
	"context"
	"errors"
	"testing"
)

type mapScope struct {
	values    map[string]Value
	aggregate func(fn, table, field string) (Value, error)
}

func (m mapScope) Lookup(path string) (Value, bool) {
	v, ok := m.values[path]
	return v, ok
}
func (m mapScope) Aggregate(fn, table, field string) (Value, error) {
	return m.aggregate(fn, table, field)
}

func TestArithmetic(t *testing.T) {
	ast := &Expr{Kind: ExprBinary, Op: "*",
		L: &Expr{Kind: ExprIdent, Name: "quantity"},
		R: &Expr{Kind: ExprIdent, Name: "unit_price"},
	}
	scope := mapScope{values: map[string]Value{"quantity": NumberValue(5), "unit_price": NumberValue(2.5)}}

	v, err := NewTreeEvaluator().Eval(context.Background(), ast, scope)
	if err != nil || v.Kind != ValueNumber || v.Number != 12.5 {
		t.Fatalf("eval = %+v err=%v", v, err)
	}
}

func TestDeterministicRepeatedEval(t *testing.T) {
	ast := &Expr{Kind: ExprBinary, Op: "+",
		L: &Expr{Kind: ExprLiteral, Value: NumberValue(1)},
		R: &Expr{Kind: ExprLiteral, Value: NumberValue(2)},
	}
	for i := 0; i < 1000; i++ {
		v, err := NewTreeEvaluator().Eval(context.Background(), ast, nil)
		if err != nil || v.Number != 3 {
			t.Fatalf("non-deterministic eval at %d: %+v err=%v", i, v, err)
		}
	}
}

func TestTypeMismatch(t *testing.T) {
	ast := &Expr{Kind: ExprBinary, Op: "+",
		L: &Expr{Kind: ExprLiteral, Value: NumberValue(1)},
		R: &Expr{Kind: ExprLiteral, Value: StringValue("x")},
	}
	_, err := NewTreeEvaluator().Eval(context.Background(), ast, nil)
	var tm *ErrExprTypeMismatch
	if !errors.As(err, &tm) {
		t.Fatalf("want ErrExprTypeMismatch, got %T: %v", err, err)
	}
}

func TestDepthExceeded(t *testing.T) {
	// Build a deep left-nested expression.
	var ast *Expr = &Expr{Kind: ExprLiteral, Value: NumberValue(1)}
	for i := 0; i < 100; i++ {
		ast = &Expr{Kind: ExprBinary, Op: "+", L: ast, R: &Expr{Kind: ExprLiteral, Value: NumberValue(1)}}
	}
	_, err := NewTreeEvaluator().Eval(context.Background(), ast, nil)
	var de *ErrExprDepthExceeded
	if !errors.As(err, &de) {
		t.Fatalf("want ErrExprDepthExceeded, got %T: %v", err, err)
	}
}

func TestSumAggregate(t *testing.T) {
	ast := &Expr{Kind: ExprAggregate, Func: "SUM", Name: "items", Args: []*Expr{{Kind: ExprIdent, Name: "amount"}}}
	scope := mapScope{aggregate: func(fn, table, field string) (Value, error) {
		if fn == "SUM" && table == "items" && field == "amount" {
			return NumberValue(42), nil
		}
		return NullValue(), nil
	}}
	v, err := NewTreeEvaluator().Eval(context.Background(), ast, scope)
	if err != nil || v.Number != 42 {
		t.Fatalf("sum = %+v err=%v", v, err)
	}
}

func TestRoundCall(t *testing.T) {
	ast := &Expr{Kind: ExprCall, Func: "ROUND", Args: []*Expr{
		{Kind: ExprLiteral, Value: NumberValue(2.567)},
		{Kind: ExprLiteral, Value: NumberValue(2)},
	}}
	v, err := NewTreeEvaluator().Eval(context.Background(), ast, nil)
	if err != nil || v.Number != 2.57 {
		t.Fatalf("round = %+v err=%v", v, err)
	}
}

func TestUnknownFunction(t *testing.T) {
	ast := &Expr{Kind: ExprCall, Func: "NOPE", Args: []*Expr{{Kind: ExprLiteral, Value: NumberValue(1)}}}
	_, err := NewTreeEvaluator().Eval(context.Background(), ast, nil)
	var uf *ErrUnknownFunction
	if !errors.As(err, &uf) {
		t.Fatalf("want ErrUnknownFunction, got %T: %v", err, err)
	}
}
