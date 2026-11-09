package expression

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

type tokenKind uint8

const (
	tokenEOF tokenKind = iota
	tokenNumber
	tokenString
	tokenIdent
	tokenOperator
	tokenLParen
	tokenRParen
	tokenComma
)

type exprToken struct {
	kind tokenKind
	text string
	pos  int
}

// Parse parses the side-effect-free expression subset understood by
// TreeEvaluator. Aggregates and unknown functions are rejected because they
// can perform I/O through Scope.Aggregate. Input length, AST nodes and depth
// are bounded before the result is returned.
func Parse(source string, maxNodes, maxDepth int) (*ParseResult, error) {
	if len(source) == 0 || len(source) > 8192 {
		return nil, fmt.Errorf("expression length must be between 1 and 8192 bytes")
	}
	if maxNodes <= 0 || maxDepth <= 0 {
		return nil, fmt.Errorf("expression node and depth limits must be positive")
	}
	p := parser{source: source, maxNodes: maxNodes, maxDepth: maxDepth}
	if err := p.next(); err != nil {
		return nil, err
	}
	ast, err := p.parseBinary(1, 0)
	if err != nil {
		return nil, err
	}
	if p.current.kind != tokenEOF {
		return nil, p.parseError(p.current.pos, "unexpected token "+strconv.Quote(p.current.text))
	}
	return &ParseResult{AST: ast, Deps: p.dependencies}, nil
}

type parser struct {
	source       string
	offset       int
	current      exprToken
	nodes        int
	maxNodes     int
	maxDepth     int
	dependencies []string
	dependency   map[string]struct{}
}

func (p *parser) next() error {
	for p.offset < len(p.source) && unicode.IsSpace(rune(p.source[p.offset])) {
		p.offset++
	}
	start := p.offset
	if start >= len(p.source) {
		p.current = exprToken{kind: tokenEOF, pos: start}
		return nil
	}
	c := p.source[p.offset]
	switch {
	case c == '(':
		p.offset++
		p.current = exprToken{kind: tokenLParen, text: "(", pos: start}
		return nil
	case c == ')':
		p.offset++
		p.current = exprToken{kind: tokenRParen, text: ")", pos: start}
		return nil
	case c == ',':
		p.offset++
		p.current = exprToken{kind: tokenComma, text: ",", pos: start}
		return nil
	case c == '"':
		p.offset++
		escaped := false
		for p.offset < len(p.source) {
			ch := p.source[p.offset]
			p.offset++
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				text := p.source[start:p.offset]
				value, err := strconv.Unquote(text)
				if err != nil {
					return p.parseError(start, "invalid string literal")
				}
				p.current = exprToken{kind: tokenString, text: value, pos: start}
				return nil
			}
		}
		return p.parseError(start, "unterminated string literal")
	case isDigit(c) || c == '.' && p.offset+1 < len(p.source) && isDigit(p.source[p.offset+1]):
		p.offset++
		for p.offset < len(p.source) {
			ch := p.source[p.offset]
			if isDigit(ch) || ch == '.' || ch == 'e' || ch == 'E' || ch == '+' || ch == '-' {
				if (ch == '+' || ch == '-') && p.offset > start && p.source[p.offset-1] != 'e' && p.source[p.offset-1] != 'E' {
					break
				}
				p.offset++
				continue
			}
			break
		}
		if _, err := strconv.ParseFloat(p.source[start:p.offset], 64); err != nil {
			return p.parseError(start, "invalid numeric literal")
		}
		p.current = exprToken{kind: tokenNumber, text: p.source[start:p.offset], pos: start}
		return nil
	case isIdentStart(c):
		p.offset++
		for p.offset < len(p.source) && isIdentContinue(p.source[p.offset]) {
			p.offset++
		}
		text := p.source[start:p.offset]
		upper := strings.ToUpper(text)
		if upper == "AND" || upper == "OR" {
			p.current = exprToken{kind: tokenOperator, text: upper, pos: start}
		} else {
			p.current = exprToken{kind: tokenIdent, text: text, pos: start}
		}
		return nil
	case strings.ContainsRune("+-*/=<>!", rune(c)):
		p.offset++
		if p.offset < len(p.source) && (p.source[p.offset] == '=' || c == '<' && p.source[p.offset] == '>' || c == '!' && p.source[p.offset] == '=') {
			p.offset++
		}
		text := p.source[start:p.offset]
		if text == "==" {
			text = "="
		}
		if text != "+" && text != "-" && text != "*" && text != "/" && text != "=" && text != "!=" && text != "<" && text != "<=" && text != ">" && text != ">=" {
			return p.parseError(start, "unsupported operator")
		}
		p.current = exprToken{kind: tokenOperator, text: text, pos: start}
		return nil
	default:
		return p.parseError(start, "unexpected character")
	}
}

func (p *parser) parseBinary(minPrecedence, depth int) (*Expr, error) {
	if depth > p.maxDepth {
		return nil, p.parseError(p.current.pos, "expression depth limit exceeded")
	}
	left, err := p.parsePrimary(depth + 1)
	if err != nil {
		return nil, err
	}
	for p.current.kind == tokenOperator {
		precedence := operatorPrecedence(p.current.text)
		if precedence < minPrecedence {
			break
		}
		op := p.current
		if err := p.next(); err != nil {
			return nil, err
		}
		right, err := p.parseBinary(precedence+1, depth+1)
		if err != nil {
			return nil, err
		}
		left, err = p.addNode(&Expr{Kind: ExprBinary, Op: op.text, L: left, R: right}, op.pos)
		if err != nil {
			return nil, err
		}
	}
	return left, nil
}

func (p *parser) parsePrimary(depth int) (*Expr, error) {
	if depth > p.maxDepth {
		return nil, p.parseError(p.current.pos, "expression depth limit exceeded")
	}
	tok := p.current
	switch tok.kind {
	case tokenOperator:
		if tok.text != "-" {
			return nil, p.parseError(tok.pos, "unary operator is not supported")
		}
		if err := p.next(); err != nil {
			return nil, err
		}
		right, err := p.parsePrimary(depth + 1)
		if err != nil {
			return nil, err
		}
		zero, err := p.addNode(&Expr{Kind: ExprLiteral, Value: NumberValue(0)}, tok.pos)
		if err != nil {
			return nil, err
		}
		return p.addNode(&Expr{Kind: ExprBinary, Op: "-", L: zero, R: right}, tok.pos)
	case tokenNumber:
		value, _ := strconv.ParseFloat(tok.text, 64)
		if math.IsInf(value, 0) || math.IsNaN(value) {
			return nil, p.parseError(tok.pos, "numeric literal is not finite")
		}
		if err := p.next(); err != nil {
			return nil, err
		}
		return p.addNode(&Expr{Kind: ExprLiteral, Value: NumberValue(value)}, tok.pos)
	case tokenString:
		if err := p.next(); err != nil {
			return nil, err
		}
		return p.addNode(&Expr{Kind: ExprLiteral, Value: StringValue(tok.text)}, tok.pos)
	case tokenIdent:
		if err := p.next(); err != nil {
			return nil, err
		}
		upper := strings.ToUpper(tok.text)
		switch upper {
		case "TRUE", "FALSE":
			return p.addNode(&Expr{Kind: ExprLiteral, Value: BoolValue(upper == "TRUE")}, tok.pos)
		case "NULL":
			return p.addNode(&Expr{Kind: ExprLiteral, Value: NullValue()}, tok.pos)
		}
		if p.current.kind == tokenLParen {
			if upper != "ROUND" {
				return nil, p.parseError(tok.pos, "function "+tok.text+" is not permitted")
			}
			return p.parseCall(tok, depth+1)
		}
		p.addDependency(tok.text)
		return p.addNode(&Expr{Kind: ExprIdent, Name: tok.text}, tok.pos)
	case tokenLParen:
		if err := p.next(); err != nil {
			return nil, err
		}
		expr, err := p.parseBinary(1, depth+1)
		if err != nil {
			return nil, err
		}
		if p.current.kind != tokenRParen {
			return nil, p.parseError(p.current.pos, "expected closing parenthesis")
		}
		if err := p.next(); err != nil {
			return nil, err
		}
		return expr, nil
	default:
		return nil, p.parseError(tok.pos, "expected a literal, identifier, or parenthesized expression")
	}
}

func (p *parser) parseCall(name exprToken, depth int) (*Expr, error) {
	if err := p.next(); err != nil {
		return nil, err
	}
	args := make([]*Expr, 0, 2)
	for {
		arg, err := p.parseBinary(1, depth+1)
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
		if len(args) > 2 {
			return nil, p.parseError(p.current.pos, "ROUND accepts at most two arguments")
		}
		if p.current.kind == tokenRParen {
			break
		}
		if p.current.kind != tokenComma {
			return nil, p.parseError(p.current.pos, "expected comma or closing parenthesis")
		}
		if err := p.next(); err != nil {
			return nil, err
		}
	}
	if err := p.next(); err != nil {
		return nil, err
	}
	if len(args) != 2 {
		return nil, p.parseError(name.pos, "ROUND requires two arguments")
	}
	return p.addNode(&Expr{Kind: ExprCall, Func: "ROUND", Args: args}, name.pos)
}

func (p *parser) addNode(node *Expr, pos int) (*Expr, error) {
	p.nodes++
	if p.nodes > p.maxNodes {
		return nil, p.parseError(pos, "expression node limit exceeded")
	}
	return node, nil
}

func (p *parser) addDependency(name string) {
	if p.dependency == nil {
		p.dependency = make(map[string]struct{})
	}
	if _, exists := p.dependency[name]; exists {
		return
	}
	p.dependency[name] = struct{}{}
	p.dependencies = append(p.dependencies, name)
}

func (p *parser) parseError(pos int, detail string) error {
	return ErrExprParse{Pos: pos, Detail: detail}
}

func operatorPrecedence(op string) int {
	switch op {
	case "OR":
		return 1
	case "AND":
		return 2
	case "=", "!=", "<", "<=", ">", ">=":
		return 3
	case "+", "-":
		return 4
	case "*", "/":
		return 5
	default:
		return 0
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool {
	return c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}
func isIdentContinue(c byte) bool { return isIdentStart(c) || isDigit(c) || c == '.' }
