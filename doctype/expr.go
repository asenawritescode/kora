// Package doctype — boolean expression engine for validation and workflow conditions.
//
// Deprecation notice (Phase 1 of RFC-0001):
// The expr-lang/expr evaluator in this file will be replaced by the Zygomys
// Lisp sandbox (lisp.go) for validation predicates and workflow conditions.
// See docs/rfc-0001-lisp-embedding.md for the full migration plan.
package doctype

import (
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

// ExprEngine compiles and evaluates constraint/workflow expressions.
// Uses expr-lang/expr for safe, sandboxed evaluation.
type ExprEngine struct {
	// Cache of compiled programs keyed by expression string.
	cache map[string]*vm.Program
}

// NewExprEngine creates a new expression engine.
func NewExprEngine() *ExprEngine {
	return &ExprEngine{
		cache: make(map[string]*vm.Program),
	}
}

// EvalEnv is the environment exposed to expression evaluation.
type EvalEnv struct {
	Doc  map[string]any `expr:"doc"`
	User *EvalUser      `expr:"user"`
}

// EvalUser exposes user info for expressions like user.has_role('Role').
type EvalUser struct {
	Name  string   `expr:"name"`
	Roles []string `expr:"roles"`
}

// Evaluate compiles (if not cached) and runs an expression against a document and user.
// Returns true if the expression evaluates to true, false otherwise.
// Expressions that fail to compile or evaluate default to false (fail-closed).
func (e *ExprEngine) Evaluate(exprStr string, doc *Document, userRoles []string) bool {
	if exprStr == "" {
		return true
	}

	// Build environment from document.
	env := EvalEnv{
		Doc: make(map[string]any),
		User: &EvalUser{
			Name:  "",
			Roles: userRoles,
		},
	}

	if doc != nil {
		for k, v := range doc.Fields {
			env.Doc[k] = normalizeExprValue(v)
		}
		env.Doc["name"] = doc.Name
		env.Doc["doc_status"] = doc.DocStatus
	}

	// Compile or retrieve from cache.
	program, err := e.compile(exprStr)
	if err != nil {
		slog.Warn("expression compilation failed, defaulting to false", "expr", exprStr, "error", err)
		return false
	}

	// Evaluate.
	output, err := expr.Run(program, env)
	if err != nil {
		slog.Warn("expression evaluation failed, defaulting to false", "expr", exprStr, "error", err)
		return false
	}

	result, ok := output.(bool)
	if !ok {
		slog.Warn("expression did not return bool, defaulting to false", "expr", exprStr, "output", output)
		return false
	}

	return result
}

func (e *ExprEngine) compile(exprStr string) (*vm.Program, error) {
	if cached, ok := e.cache[exprStr]; ok {
		return cached, nil
	}

	// Build options with custom functions.
	opts := []expr.Option{
		expr.Env(EvalEnv{}),
		expr.Function("today", func(params ...any) (any, error) {
			now := time.Now()
			return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()), nil
		}),
		expr.Function("now", func(params ...any) (any, error) {
			return time.Now(), nil
		}),
		expr.Function("len", func(params ...any) (any, error) {
			if len(params) != 1 {
				return 0, fmt.Errorf("len() takes exactly 1 argument")
			}
			switch v := params[0].(type) {
			case string:
				return len(v), nil
			case []any:
				return len(v), nil
			case []*Document:
				return len(v), nil
			default:
				return 0, nil
			}
		}),
		expr.Function("has_role", func(params ...any) (any, error) {
			// user.has_role('Role') — called on the user object via method.
			// This is handled through the EvalUser type below.
			return false, nil
		}),
	}

	program, err := expr.Compile(exprStr, opts...)
	if err != nil {
		return nil, fmt.Errorf("compiling expression %q: %w", exprStr, err)
	}

	e.cache[exprStr] = program
	return program, nil
}

// normalizeExprValue converts string values from the database to numeric types
// so that expressions like doc.total > 0 work correctly.
func normalizeExprValue(v any) any {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case string:
		// Try int first, then float.
		if i, err := strconv.ParseInt(val, 10, 64); err == nil {
			return float64(i)
		}
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			return f
		}
		return val
	case []byte:
		return normalizeExprValue(string(val))
	default:
		return v
	}
}

// EvalUser method for has_role. The expr library supports calling methods on structs.
func (u *EvalUser) Has_role(role string) bool {
	for _, r := range u.Roles {
		if r == role || r == AdminRole {
			return true
		}
	}
	return false
}

// DefaultEngine is a package-level engine used by validation and workflow code.
var DefaultEngine = NewExprEngine()

// evaluateCondition evaluates a condition expression using the expr engine.
// This replaces the old hand-rolled string matcher.
func evaluateCondition(exprStr string, doc *Document, user any) bool {
	userRoles := []string{AdminRole}
	if u, ok := user.(*EvalUser); ok && u != nil {
		userRoles = u.Roles
	}
	if userRoles == nil {
		userRoles = []string{}
	}
	return DefaultEngine.Evaluate(exprStr, doc, userRoles)
}
