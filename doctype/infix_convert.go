package doctype

import (
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

var arithPattern = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*|\d+\.?\d*)\s*([+\-*/])\s+(.+)$`)

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

func isFieldRef(s string) bool {
	// today() is a function call, not a field reference.
	if s == "today()" {
		return false
	}
	if len(s) == 0 {
		return false
	}
	return (s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z') || s[0] == '_'
}
