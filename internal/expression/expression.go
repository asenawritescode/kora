// Package expression defines the single shared expression and rule evaluation
// contract (PLUGIN-007). Every feature — computed fields, constraints, workflow
// preconditions, and rules — must evaluate through one deterministic,
// side-effect-free Evaluator over a typed AST and a typed Value model.
package expression

import (
	"context"
	"fmt"
)

// ValueKind classifies a typed value. No map[string]any crosses this boundary.
type ValueKind string

const (
	ValueNumber ValueKind = "number"
	ValueString ValueKind = "string"
	ValueBool   ValueKind = "bool"
	ValueNull   ValueKind = "null"
)

// Value is a typed expression value.
type Value struct {
	Kind   ValueKind
	Number float64
	String string
	Bool   bool
}

// NumberValue builds a numeric Value.
func NumberValue(n float64) Value { return Value{Kind: ValueNumber, Number: n} }

// BoolValue builds a boolean Value.
func BoolValue(b bool) Value { return Value{Kind: ValueBool, Bool: b} }

// StringValue builds a string Value.
func StringValue(s string) Value { return Value{Kind: ValueString, String: s} }

// NullValue builds a null Value.
func NullValue() Value { return Value{Kind: ValueNull} }

// ExprKind classifies an AST node.
type ExprKind string

const (
	ExprLiteral   ExprKind = "literal"
	ExprIdent     ExprKind = "ident"
	ExprBinary    ExprKind = "binary"
	ExprCall      ExprKind = "call"      // ROUND(expr, N)
	ExprAggregate ExprKind = "aggregate" // SUM(table.field)
)

// Expr is a typed AST node.
type Expr struct {
	Kind ExprKind

	// Literal
	Value Value
	// Ident
	Name string
	// Binary
	Op   string // + - * / = != < <= > >= AND OR
	L, R *Expr
	// Call / Aggregate
	Func string // ROUND | SUM
	Args []*Expr
}

// ParseResult is the outcome of parsing an expression source.
type ParseResult struct {
	AST    *Expr
	Deps   []string
	Errors []ExprError
}

// Scope resolves identifiers and aggregates. Lookup returns typed values only.
type Scope interface {
	Lookup(path string) (Value, bool)
	Aggregate(fn, table, field string) (Value, error)
}

// Evaluator evaluates an AST against a scope, deterministically and with no
// side effects.
type Evaluator interface {
	Eval(ctx context.Context, ast *Expr, scope Scope) (Value, error)
}

// ExprError is a typed expression error.
type ExprError struct {
	Pos    int
	Detail string
}

func (e ExprError) Error() string { return fmt.Sprintf("expr: %s (pos %d)", e.Detail, e.Pos) }

// ErrExprParse is returned for parse failures.
type ErrExprParse struct {
	Pos    int
	Detail string
}

func (e ErrExprParse) Error() string { return fmt.Sprintf("expr parse: %s (pos %d)", e.Detail, e.Pos) }

// ErrExprTypeMismatch reports an operation on incompatible value kinds.
type ErrExprTypeMismatch struct {
	Got, Want string
}

func (e ErrExprTypeMismatch) Error() string {
	return fmt.Sprintf("expr type mismatch: got %s, want %s", e.Got, e.Want)
}

// ErrExprDepthExceeded reports an AST deeper than the allowed limit.
type ErrExprDepthExceeded struct {
	Limit int
}

func (e ErrExprDepthExceeded) Error() string {
	return fmt.Sprintf("expr depth exceeded limit %d", e.Limit)
}

// ErrUnknownFunction reports a call to a function outside the closed registry.
type ErrUnknownFunction struct {
	Name string
}

func (e ErrUnknownFunction) Error() string { return "expr unknown function " + e.Name }
