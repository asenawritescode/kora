package doctype

import (
	"strings"

	"github.com/asenawritescode/kora/internal/numeric"
	"github.com/shopspring/decimal"
)

// evalDecimalSExpr evaluates the arithmetic subset of computed expressions
// (+ - * /, sum, round, numeric literals and field references) with exact
// decimals. It reports false for anything outside that subset, or for empty
// operands, so the caller can fall back to the Lisp sandbox unchanged.
func evalDecimalSExpr(expr string, doc *Document) (decimal.Decimal, bool) {
	tokens := tokenizeSExpr(expr)
	node, rest, ok := parseSExprNode(tokens)
	if !ok || len(rest) != 0 {
		return decimal.Decimal{}, false
	}
	return evalDecimalNode(node, doc)
}

type sexprNode struct {
	atom   string
	quoted bool
	list   []sexprNode
	isList bool
}

func tokenizeSExpr(expr string) []string {
	var tokens []string
	for i := 0; i < len(expr); {
		switch c := expr[i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(' || c == ')':
			tokens = append(tokens, string(c))
			i++
		case c == '"':
			end := strings.IndexByte(expr[i+1:], '"')
			if end < 0 {
				return nil
			}
			tokens = append(tokens, expr[i:i+end+2])
			i += end + 2
		default:
			start := i
			for i < len(expr) && !strings.ContainsRune(" \t\n\r()\"", rune(expr[i])) {
				i++
			}
			tokens = append(tokens, expr[start:i])
		}
	}
	return tokens
}

func parseSExprNode(tokens []string) (sexprNode, []string, bool) {
	if len(tokens) == 0 {
		return sexprNode{}, nil, false
	}
	head, rest := tokens[0], tokens[1:]
	switch {
	case head == ")":
		return sexprNode{}, nil, false
	case head == "(":
		node := sexprNode{isList: true}
		for len(rest) > 0 && rest[0] != ")" {
			child, remaining, ok := parseSExprNode(rest)
			if !ok {
				return sexprNode{}, nil, false
			}
			node.list = append(node.list, child)
			rest = remaining
		}
		if len(rest) == 0 {
			return sexprNode{}, nil, false
		}
		return node, rest[1:], true
	case strings.HasPrefix(head, `"`):
		return sexprNode{atom: strings.Trim(head, `"`), quoted: true}, rest, true
	}
	return sexprNode{atom: head}, rest, true
}

func evalDecimalNode(node sexprNode, doc *Document) (decimal.Decimal, bool) {
	if !node.isList {
		if node.quoted {
			return decimal.Decimal{}, false
		}
		if d, err := decimal.NewFromString(node.atom); err == nil {
			return d, true
		}
		return numeric.Parse(doc.Get(node.atom))
	}
	if len(node.list) == 0 || node.list[0].isList {
		return decimal.Decimal{}, false
	}
	op, args := node.list[0].atom, node.list[1:]
	switch op {
	case "sum":
		if len(args) != 2 || !args[0].quoted || !args[1].quoted {
			return decimal.Decimal{}, false
		}
		total := decimal.Zero
		for _, row := range doc.GetTable(args[0].atom) {
			value := row.Get(args[1].atom)
			if value == nil || value == "" {
				continue
			}
			d, ok := numeric.Parse(value)
			if !ok {
				return decimal.Decimal{}, false
			}
			total = total.Add(d)
		}
		return total, true
	case "round":
		if len(args) != 2 {
			return decimal.Decimal{}, false
		}
		value, ok := evalDecimalNode(args[0], doc)
		places, err := decimal.NewFromString(args[1].atom)
		if !ok || err != nil || !places.IsInteger() {
			return decimal.Decimal{}, false
		}
		return value.Round(int32(places.IntPart())), true
	case "+", "-", "*", "/":
		if len(args) < 2 {
			return decimal.Decimal{}, false
		}
		acc, ok := evalDecimalNode(args[0], doc)
		if !ok {
			return decimal.Decimal{}, false
		}
		for _, arg := range args[1:] {
			next, ok := evalDecimalNode(arg, doc)
			if !ok {
				return decimal.Decimal{}, false
			}
			switch op {
			case "+":
				acc = acc.Add(next)
			case "-":
				acc = acc.Sub(next)
			case "*":
				acc = acc.Mul(next)
			case "/":
				if next.IsZero() {
					return decimal.Decimal{}, false
				}
				acc = acc.Div(next)
			}
		}
		return acc, true
	}
	return decimal.Decimal{}, false
}
