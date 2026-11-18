package doctype

import (
	"fmt"
	"regexp"
	"strings"
)

// InfixToSExpr converts a legacy infix computed field expression to
// an s-expression that the Zygomys Lisp sandbox can evaluate.
// If the expression is already an s-expression (starts with '('), it's returned as-is.
func InfixToSExpr(expr string) string {
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, "(") {
		return expr // already an s-expression
	}
	if sexpr, ok := infixCondition(expr).aggregateCompare(); ok {
		return sexpr
	}
	if strings.HasPrefix(expr, "@script:") {
		return expr // script hooks pass through unchanged
	}

	// Apply conversions from outermost to innermost.
	converted := expr

	// 0. today() → (today)
	converted = strings.ReplaceAll(converted, "today()", "(today)")

	// 1. SUM(field.column) → (sum "field" "column")
	converted = sumPattern.ReplaceAllStringFunc(converted, func(m string) string {
		parts := sumPattern.FindStringSubmatch(m)
		return `(sum "` + parts[1] + `" "` + parts[2] + `")`
	})

	// 2. COUNT(field) → (count "field")
	converted = countPattern.ReplaceAllStringFunc(converted, func(m string) string {
		parts := countPattern.FindStringSubmatch(m)
		return `(count "` + parts[1] + `")`
	})

	// 3. DATEDIFF(a, b) → (datediff a b)
	// Field references are passed as symbols (looked up in doc fields).
	// Call expressions like today() are converted to (today).
	converted = datediffPattern.ReplaceAllStringFunc(converted, func(m string) string {
		parts := datediffPattern.FindStringSubmatch(m)
		a, b := strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])
		// Convert function calls but leave field names as symbols.
		a = strings.ReplaceAll(a, "today()", "(today)")
		b = strings.ReplaceAll(b, "today()", "(today)")
		return "(datediff " + a + " " + b + ")"
	})

	// 4. ROUND(inner, N) → (round inner N)
	converted = roundPattern.ReplaceAllStringFunc(converted, func(m string) string {
		parts := roundPattern.FindStringSubmatch(m)
		inner := strings.TrimSpace(parts[1])
		decimals := strings.TrimSpace(parts[2])
		// Recursively convert the inner expression.
		inner = InfixToSExpr(inner)
		return "(round " + inner + " " + decimals + ")"
	})

	// 5. Arithmetic operators with spaces around them.
	// Process right-to-left for correct associativity of - and /.
	converted = convertArithmetic(converted)

	return converted
}

func convertArithmetic(expr string) string {
	// Find the last top-level operator (right-associative for -, /).
	// Simple approach: split on operators with spaces.
	// For now, handle the most common patterns: a + b, a - b, a * b, a / b.
	parts := strings.Fields(expr)
	if len(parts) < 3 {
		// Single token — might be a field reference or number.
		return expr
	}

	// Find the rightmost operator with lowest precedence (+ or -).
	opIdx := -1
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] == "+" || parts[i] == "-" {
			opIdx = i
			break
		}
	}
	if opIdx < 0 {
		// No + or -, look for * or /.
		for i := len(parts) - 1; i >= 0; i-- {
			if parts[i] == "*" || parts[i] == "/" {
				opIdx = i
				break
			}
		}
	}
	if opIdx < 0 {
		return expr
	}

	left := strings.Join(parts[:opIdx], " ")
	op := toLispOp(parts[opIdx])
	right := strings.Join(parts[opIdx+1:], " ")

	left = convertArithmetic(left)
	right = convertArithmetic(right)

	return "(" + op + " " + left + " " + right + ")"
}

func toLispOp(op string) string {
	switch op {
	case "+":
		return "+"
	case "-":
		return "-"
	case "*":
		return "*"
	case "/":
		return "/"
	}
	return op
}

// aggregateComparePattern matches one comparison between a SUM or COUNT over a
// child table and a field or number, e.g. SUM(items.amount) = total.
var aggregateComparePattern = regexp.MustCompile(`^\s*((?:SUM|COUNT)\(\s*\w+(?:\.\w+)?\s*\))\s*(==|!=|<=|>=|<|>|=)\s*([A-Za-z_]\w*|-?[0-9.]+)\s*$`)

// infixCondition is a doc-constraint condition written in infix form.
type infixCondition string

// aggregateCompare converts one infix comparison of a child-table aggregate to
// an s-expression. "=" is equality, as in "==".
func (c infixCondition) aggregateCompare() (string, bool) {
	m := aggregateComparePattern.FindStringSubmatch(string(c))
	if m == nil {
		return "", false
	}
	op := m[2]
	if op == "=" {
		op = "=="
	}
	return "(" + op + " " + InfixToSExpr(m[1]) + " " + m[3] + ")", true
}

// hasAggregate reports whether an infix condition sums or counts child rows.
func (c infixCondition) hasAggregate() bool {
	return sumPattern.MatchString(string(c)) || countPattern.MatchString(string(c))
}

// bareEquals matches a single = that is not part of ==, !=, <= or >=.
var bareEquals = regexp.MustCompile(`[^=!<>]=[^=]|^=[^=]`)

// validateForm rejects an infix condition that sums or counts child rows in a
// form the evaluator cannot read. Only one comparison of an aggregate against a
// field or number is accepted, or an s-expression written by hand.
func (c infixCondition) validateForm() error {
	condition := string(c)
	if strings.HasPrefix(strings.TrimSpace(condition), "(") {
		return nil
	}
	if bareEquals.MatchString(condition) {
		return fmt.Errorf("condition %q uses = for equality; write == instead", condition)
	}
	if !c.hasAggregate() {
		return nil
	}
	if _, ok := c.aggregateCompare(); ok {
		return nil
	}
	return fmt.Errorf("condition %q sums child rows in an unsupported infix form; write one comparison such as SUM(table.field) = total, or an s-expression such as (== (sum \"table\" \"field\") total)", condition)
}
