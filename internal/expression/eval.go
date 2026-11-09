package expression

import (
	"context"
	"math"
)

// TreeEvaluator is a deterministic, depth-limited Evaluator. It performs no
// clock, randomness, or I/O; evaluation order is fixed by the AST.
type TreeEvaluator struct {
	MaxDepth int
}

// NewTreeEvaluator returns an evaluator with a default depth limit of 64.
func NewTreeEvaluator() *TreeEvaluator {
	return &TreeEvaluator{MaxDepth: 64}
}

// Eval evaluates ast against scope.
func (e *TreeEvaluator) Eval(ctx context.Context, ast *Expr, scope Scope) (Value, error) {
	if ast == nil {
		return NullValue(), nil
	}
	if e.MaxDepth <= 0 {
		e.MaxDepth = 64
	}
	return e.eval(ctx, ast, scope, 0)
}

func (e *TreeEvaluator) eval(ctx context.Context, ast *Expr, scope Scope, depth int) (Value, error) {
	if depth > e.MaxDepth {
		return Value{}, &ErrExprDepthExceeded{Limit: e.MaxDepth}
	}
	switch ast.Kind {
	case ExprLiteral:
		return ast.Value, nil
	case ExprIdent:
		if scope == nil {
			return NullValue(), nil
		}
		if v, ok := scope.Lookup(ast.Name); ok {
			return v, nil
		}
		return NullValue(), nil
	case ExprBinary:
		l, err := e.eval(ctx, ast.L, scope, depth+1)
		if err != nil {
			return Value{}, err
		}
		r, err := e.eval(ctx, ast.R, scope, depth+1)
		if err != nil {
			return Value{}, err
		}
		return evalBinary(ast.Op, l, r)
	case ExprCall:
		if len(ast.Args) == 0 {
			return Value{}, &ErrExprTypeMismatch{Got: "no args", Want: "1-2 args"}
		}
		first, err := e.eval(ctx, ast.Args[0], scope, depth+1)
		if err != nil {
			return Value{}, err
		}
		switch ast.Func {
		case "ROUND":
			if len(ast.Args) < 2 {
				return Value{}, &ErrExprTypeMismatch{Got: "1 arg", Want: "2 args for ROUND"}
			}
			n, err := e.eval(ctx, ast.Args[1], scope, depth+1)
			if err != nil {
				return Value{}, err
			}
			return roundValue(first, n)
		default:
			return Value{}, &ErrUnknownFunction{Name: ast.Func}
		}
	case ExprAggregate:
		if scope == nil {
			return NullValue(), nil
		}
		if len(ast.Args) != 1 || ast.Args[0].Kind != ExprIdent {
			return Value{}, &ErrExprTypeMismatch{Got: "bad aggregate args", Want: "SUM(ident)"}
		}
		return scope.Aggregate(ast.Func, ast.Name, ast.Args[0].Name)
	default:
		return Value{}, &ErrExprTypeMismatch{Got: string(ast.Kind), Want: "known expr kind"}
	}
}

func evalBinary(op string, l, r Value) (Value, error) {
	switch op {
	case "+", "-", "*", "/":
		if l.Kind != ValueNumber || r.Kind != ValueNumber {
			return Value{}, &ErrExprTypeMismatch{Got: string(l.Kind) + "," + string(r.Kind), Want: "number,number"}
		}
		switch op {
		case "+":
			return NumberValue(l.Number + r.Number), nil
		case "-":
			return NumberValue(l.Number - r.Number), nil
		case "*":
			return NumberValue(l.Number * r.Number), nil
		case "/":
			if r.Number == 0 {
				return Value{}, &ErrExprTypeMismatch{Got: "division by zero", Want: "non-zero divisor"}
			}
			return NumberValue(l.Number / r.Number), nil
		}
	case "=", "!=", "<", "<=", ">", ">=":
		switch {
		case l.Kind == ValueNumber && r.Kind == ValueNumber:
			return BoolValue(compareNumbers(op, l.Number, r.Number)), nil
		case l.Kind == ValueString && r.Kind == ValueString:
			return BoolValue(compareStrings(op, l.String, r.String)), nil
		case l.Kind == ValueBool && r.Kind == ValueBool:
			if op == "=" {
				return BoolValue(l.Bool == r.Bool), nil
			}
			if op == "!=" {
				return BoolValue(l.Bool != r.Bool), nil
			}
			return Value{}, &ErrExprTypeMismatch{Got: "bool", Want: "number or string for ordering"}
		default:
			return Value{}, &ErrExprTypeMismatch{Got: string(l.Kind), Want: string(r.Kind)}
		}
	case "AND", "OR":
		if l.Kind != ValueBool || r.Kind != ValueBool {
			return Value{}, &ErrExprTypeMismatch{Got: string(l.Kind) + "," + string(r.Kind), Want: "bool,bool"}
		}
		if op == "AND" {
			return BoolValue(l.Bool && r.Bool), nil
		}
		return BoolValue(l.Bool || r.Bool), nil
	}
	return Value{}, &ErrUnknownFunction{Name: op}
}

func compareNumbers(op string, a, b float64) bool {
	switch op {
	case "=":
		return a == b
	case "!=":
		return a != b
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

func compareStrings(op string, a, b string) bool {
	switch op {
	case "=":
		return a == b
	case "!=":
		return a != b
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

func roundValue(v Value, n Value) (Value, error) {
	if v.Kind != ValueNumber || n.Kind != ValueNumber {
		return Value{}, &ErrExprTypeMismatch{Got: string(v.Kind) + "," + string(n.Kind), Want: "number,number"}
	}
	places := int(n.Number)
	scale := math.Pow(10, float64(places))
	return NumberValue(math.Round(v.Number*scale) / scale), nil
}
