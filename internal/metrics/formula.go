package metrics

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/shopspring/decimal"
)

// formulaToken is one number, metric name or operator. Any other text makes the formula invalid.
var formulaToken = regexp.MustCompile(`\A\s*(\d+(?:\.\d+)?|[A-Za-z_]\w*|[-+*/()])`)

// formulaPrecedence orders the binary operators; all are left-associative.
var formulaPrecedence = map[string]int{"+": 1, "-": 1, "*": 2, "/": 2}

// formulaNode is a parsed formula. A leaf (op 0) is a number or a metric reference; a reference
// reads slot, the index of that metric in the plan's computed values. An operator node combines
// two operands.
type formulaNode struct {
	op          byte
	value       decimal.Decimal
	ref         string
	slot        int
	left, right *formulaNode
}

// parseFormula parses + - * /, parentheses, numbers and metric names. Anything else is an error,
// so a formula can never run arbitrary code.
func parseFormula(source string) (*formulaNode, error) {
	var tokens []string
	for rest := source; strings.TrimSpace(rest) != ""; {
		match := formulaToken.FindStringSubmatch(rest)
		if match == nil {
			return nil, fmt.Errorf("unsupported text in formula %q", source)
		}
		tokens = append(tokens, match[1])
		rest = rest[len(match[0]):]
	}
	parser := &formulaParser{tokens: tokens}
	root, err := parser.binary(1)
	if err != nil {
		return nil, fmt.Errorf("formula %q: %w", source, err)
	}
	if parser.peek() != "" {
		return nil, fmt.Errorf("formula %q: unexpected %q", source, parser.peek())
	}
	return root, nil
}

type formulaParser struct {
	tokens []string
	pos    int
}

func (p *formulaParser) peek() string {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return ""
}

func (p *formulaParser) next() string {
	token := p.peek()
	if token != "" {
		p.pos++
	}
	return token
}

// binary parses operators of at least minPrec (precedence climbing).
func (p *formulaParser) binary(minPrec int) (*formulaNode, error) {
	left, err := p.primary()
	if err != nil {
		return nil, err
	}
	for {
		prec, isOperator := formulaPrecedence[p.peek()]
		if !isOperator || prec < minPrec {
			return left, nil
		}
		op := p.next()
		right, err := p.binary(prec + 1)
		if err != nil {
			return nil, err
		}
		left = &formulaNode{op: op[0], left: left, right: right}
	}
}

func (p *formulaParser) primary() (*formulaNode, error) {
	token := p.next()
	switch {
	case token == "":
		return nil, fmt.Errorf("formula ends early")
	case token == "(":
		inner, err := p.binary(1)
		if err != nil {
			return nil, err
		}
		if p.next() != ")" {
			return nil, fmt.Errorf("formula is missing a closing parenthesis")
		}
		return inner, nil
	case token[0] >= '0' && token[0] <= '9':
		value, err := decimal.NewFromString(token)
		if err != nil {
			return nil, fmt.Errorf("invalid number %q in formula", token)
		}
		return &formulaNode{value: value}, nil
	case token[0] == '_' || token[0] >= 'A' && token[0] <= 'Z' || token[0] >= 'a' && token[0] <= 'z':
		return &formulaNode{ref: token}, nil
	}
	return nil, fmt.Errorf("unexpected %q in formula", token)
}

// bind sets the slot of every metric reference from slots, which maps metric name to index.
func (n *formulaNode) bind(slots map[string]int) {
	if n.op == 0 {
		if n.ref != "" {
			n.slot = slots[n.ref]
		}
		return
	}
	n.left.bind(slots)
	n.right.bind(slots)
}

// eval computes the formula over the computed values of a row. A null operand or a division by
// zero gives null, the same rule as a ratio, rather than an error or Inf.
func (n *formulaNode) eval(vals []decimal.NullDecimal) decimal.NullDecimal {
	if n.op == 0 {
		if n.ref != "" {
			return vals[n.slot]
		}
		return decimal.NullDecimal{Decimal: n.value, Valid: true}
	}
	left, right := n.left.eval(vals), n.right.eval(vals)
	if !left.Valid || !right.Valid || n.op == '/' && right.Decimal.IsZero() {
		return decimal.NullDecimal{}
	}
	var value decimal.Decimal
	switch n.op {
	case '+':
		value = left.Decimal.Add(right.Decimal)
	case '-':
		value = left.Decimal.Sub(right.Decimal)
	case '*':
		value = left.Decimal.Mul(right.Decimal)
	default:
		value = left.Decimal.Div(right.Decimal)
	}
	return decimal.NullDecimal{Decimal: value, Valid: true}
}
