// Package doctype — boolean expression engine for validation and workflow conditions.
//
// Uses the Zygomys Lisp sandbox (lisp.go) for condition evaluation.
package doctype

import (
	"fmt"
	"log/slog"
	"strings"
)

// ExprEngine compiles and evaluates constraint/workflow expressions
// using the Zygomys Lisp sandbox.
type ExprEngine struct{}

// NewExprEngine creates a new expression engine.
func NewExprEngine() *ExprEngine {
	return &ExprEngine{}
}

// Evaluate evaluates a condition expression against a document and user roles.
// Non-zero result = true, zero/nil = false. Errors default to false (fail-closed).
func (e *ExprEngine) Evaluate(exprStr string, doc *Document, userRoles []string) bool {
	return evaluateCondition(exprStr, doc, userRoles)
}

// evaluateCondition evaluates a condition expression.
func evaluateCondition(exprStr string, doc *Document, userRoles []string) bool {
	if exprStr == "" {
		return true
	}

	docFields := make(map[string]any)
	if doc != nil {
		for k, v := range doc.Fields {
			docFields[k] = normalizeExprValue(v)
		}
		docFields["name"] = doc.Name
		docFields["doc_status"] = doc.DocStatus
	}
	if len(userRoles) > 0 {
		docFields["user"] = map[string]any{
			"name":  "",
			"roles": userRoles,
		}
	}

	// Handle legacy expr-lang patterns.
	if !strings.HasPrefix(exprStr, "(") {
		if handled, result := evalLegacyEquals(exprStr, docFields); handled {
			return result
		}
		if handled, result := evalLegacyHasRole(exprStr, userRoles); handled {
			return result
		}
	}

	// Convert legacy infix or evaluate directly through Lisp sandbox.
	evalExpr := exprStr
	if !strings.HasPrefix(exprStr, "(") {
		evalExpr = InfixToSExpr(exprStr)
	}

	s := NewLispSandbox()
	defer s.Close()

	result, err := s.Eval(evalExpr, docFields, nil)
	if err != nil {
		slog.Warn("condition evaluation failed, defaulting to false", "expr", exprStr, "error", err)
		return false
	}
	return isTruthy(result)
}

// evalLegacyEquals handles doc.field == "value" and doc.field != "value".
func evalLegacyEquals(exprStr string, docFields map[string]any) (bool, bool) {
	clean := strings.ReplaceAll(exprStr, "doc.", "")
	if idx := strings.Index(clean, "=="); idx >= 0 {
		left := strings.TrimSpace(clean[:idx])
		right := strings.TrimSpace(clean[idx+2:])
		right = strings.Trim(right, `"'`)
		if val, ok := docFields[left]; ok {
			return true, fmt.Sprint(val) == right
		}
	}
	if idx := strings.Index(clean, "!="); idx >= 0 {
		left := strings.TrimSpace(clean[:idx])
		right := strings.TrimSpace(clean[idx+2:])
		right = strings.Trim(right, `"'`)
		if val, ok := docFields[left]; ok {
			return true, fmt.Sprint(val) != right
		}
	}
	return false, false
}

// evalLegacyHasRole handles user.has_role('Role').
func evalLegacyHasRole(expr string, userRoles []string) (bool, bool) {
	if !strings.Contains(expr, "has_role") {
		return false, false
	}
	for _, role := range userRoles {
		if strings.Contains(expr, fmt.Sprintf("'%s'", role)) || strings.Contains(expr, fmt.Sprintf("%q", role)) {
			return true, true
		}
	}
	return true, false
}

// isTruthy returns true for non-zero, non-nil, non-empty values.
func isTruthy(v any) bool {
	if v == nil {
		return false
	}
	switch n := v.(type) {
	case float64:
		return n != 0
	case int:
		return n != 0
	case int64:
		return n != 0
	case bool:
		return n
	case string:
		return n != "" && n != "false" && n != "0"
	}
	return true
}

// normalizeExprValue converts string DB values to float64 for numeric comparisons.
// Non-numeric strings are passed through unchanged.
func normalizeExprValue(v any) any {
	if v == nil {
		return 0.0
	}
	switch n := v.(type) {
	case float64, float32, int, int64:
		return n
	case string:
		if isNumeric(n) {
			if f, err := stringToFloat(n); err == nil {
				return f
			}
		}
		return n
	case []byte:
		s := string(n)
		if isNumeric(s) {
			if f, err := stringToFloat(s); err == nil {
				return f
			}
		}
		return s
	}
	return v
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	start := 0
	if s[0] == '-' {
		start = 1
	}
	if start >= len(s) {
		return false
	}
	hasDot := false
	hasDigit := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			hasDigit = true
			continue
		}
		if c == '.' && !hasDot {
			hasDot = true
			continue
		}
		return false
	}
	return hasDigit
}

func stringToFloat(s string) (float64, error) {
	var result float64
	var decimal float64 = 1
	var negative bool
	seenDecimal := false
	started := false

	for i, c := range s {
		if i == 0 && c == '-' {
			negative = true
			continue
		}
		if c == '.' && !seenDecimal {
			seenDecimal = true
			continue
		}
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid char in %q", s)
		}
		started = true
		if seenDecimal {
			decimal *= 10
			result += float64(c-'0') / decimal
		} else {
			result = result*10 + float64(c-'0')
		}
	}
	if !started {
		return 0, fmt.Errorf("no digits in %q", s)
	}
	if negative {
		result = -result
	}
	return result, nil
}

// DefaultEngine is the package-level expression engine used by validation and workflow.
var DefaultEngine = NewExprEngine()
