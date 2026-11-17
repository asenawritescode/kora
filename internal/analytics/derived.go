package analytics

import (
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
)

// formulaToken is one number, measure reference (`measure` or `query.measure`)
// or operator. Any other text makes the formula invalid.
var formulaToken = regexp.MustCompile(`\A\s*(\d+(?:\.\d+)?|[A-Za-z_]\w*(?:\.\w+)?|[-+*/()])`)

// formulaPrecedence orders the binary operators; all are left-associative.
var formulaPrecedence = map[string]int{"+": 1, "-": 1, "*": 2, "/": 2}

// formulaNode is a parsed formula. A leaf (op 0) is a number or a measure
// reference; an operator node combines two operands.
type formulaNode struct {
	op          byte
	value       float64
	ref         string
	left, right *formulaNode
}

// parseFormula parses `+ - * /`, parentheses, numbers and measure references.
// Anything else is an error, so a formula can never run arbitrary code.
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
		return nil, err
	}
	if parser.peek() != "" {
		return nil, fmt.Errorf("unexpected %q in formula %q", parser.peek(), source)
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
		value, err := strconv.ParseFloat(token, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid number %q in formula", token)
		}
		return &formulaNode{value: value}, nil
	case token[0] == '_' || token[0] >= 'A' && token[0] <= 'Z' || token[0] >= 'a' && token[0] <= 'z':
		return &formulaNode{ref: token}, nil
	}
	return nil, fmt.Errorf("unexpected %q in formula", token)
}

// resolves reports whether every measure reference is a key of env.
func (n *formulaNode) resolves(env map[string]*float64) bool {
	if n.op == 0 {
		_, known := env[n.ref]
		return n.ref == "" || known
	}
	return n.left.resolves(env) && n.right.resolves(env)
}

// eval computes the formula over env. A nil result is SQL-style null: a null
// operand or a division by zero yields null rather than an error or Inf.
func (n *formulaNode) eval(env map[string]*float64) *float64 {
	if n.op == 0 {
		if n.ref != "" {
			return env[n.ref]
		}
		value := n.value
		return &value
	}
	left, right := n.left.eval(env), n.right.eval(env)
	if left == nil || right == nil || n.op == '/' && *right == 0 {
		return nil
	}
	var value float64
	switch n.op {
	case '+':
		value = *left + *right
	case '-':
		value = *left - *right
	case '*':
		value = *left * *right
	default:
		value = *left / *right
	}
	return &value
}

// derivedFormula is a parsed report derived metric, ready to evaluate.
type derivedFormula struct {
	name string
	root *formulaNode
}

// parseDerivedFormulas parses each derived metric. A formula outside the
// supported grammar (for example one using if()) is skipped with a warning, so
// existing report packs keep loading and serving.
func parseDerivedFormulas(metrics []DerivedMetric) []derivedFormula {
	formulas := make([]derivedFormula, 0, len(metrics))
	for _, metric := range metrics {
		root, err := parseFormula(metric.Formula)
		if err != nil {
			slog.Warn("derived metric not evaluated", "metric", metric.Name, "error", err)
			continue
		}
		formulas = append(formulas, derivedFormula{name: metric.Name, root: root})
	}
	return formulas
}

// formulaEnv maps each measure of one query as `measure` and `query.measure`.
// A query without an id has only the bare names.
func formulaEnv(queryID string, measures []string, value func(measure string) *float64) map[string]*float64 {
	env := make(map[string]*float64, 2*len(measures))
	for _, measure := range measures {
		env[measure] = value(measure)
		if queryID != "" {
			env[queryID+"."+measure] = env[measure]
		}
	}
	return env
}

func noValue(string) *float64 { return nil }

// numberPointer reads a measure value; a missing or non-numeric value is null.
func numberPointer(raw any) *float64 {
	value, ok := semanticNumber(raw)
	if !ok {
		return nil
	}
	return &value
}

// pointerValue unwraps a nullable number for a row: nil stays a null value.
func pointerValue(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

// applyDerivedMetrics evaluates the request's derived metrics for each query
// whose measures they reference: on every row, on the comparison totals and on
// the previous-window rows. Each derived name becomes a column of that query.
func applyDerivedMetrics(response *SemanticQueryResponse, request SemanticRequest) {
	formulas := parseDerivedFormulas(request.Derived)
	for i := range response.Queries {
		if i < len(request.Queries) {
			applyDerivedToQuery(formulas, request.Queries[i], &response.Queries[i])
		}
	}
}

func applyDerivedToQuery(formulas []derivedFormula, query ModelQuery, result *SemanticQueryResult) {
	for _, formula := range formulas {
		// Formulas that reach across queries are not evaluated per query.
		if !formula.root.resolves(formulaEnv(query.ID, query.Measures, noValue)) {
			continue
		}
		for _, row := range result.Rows {
			row[formula.name] = pointerValue(formula.root.eval(rowEnv(query, row)))
		}
		if comparison := result.Comparison; comparison != nil {
			for _, row := range comparison.PreviousRows {
				row[formula.name] = pointerValue(formula.root.eval(rowEnv(query, row)))
			}
			applyDerivedToTotals(formula, query, comparison)
		}
		result.Columns = append(result.Columns, formula.name)
	}
}

func rowEnv(query ModelQuery, row map[string]any) map[string]*float64 {
	return formulaEnv(query.ID, query.Measures, func(measure string) *float64 { return numberPointer(row[measure]) })
}

// applyDerivedToTotals fills the derived total for both windows and its delta.
func applyDerivedToTotals(formula derivedFormula, query ModelQuery, comparison *SemanticComparison) {
	totals := comparison.Totals
	current := formula.root.eval(formulaEnv(query.ID, query.Measures, func(measure string) *float64 { return totals.Current[measure] }))
	previous := formula.root.eval(formulaEnv(query.ID, query.Measures, func(measure string) *float64 { return totals.Previous[measure] }))
	totals.Current[formula.name] = current
	totals.Previous[formula.name] = previous
	totals.Delta[formula.name] = semanticDelta(current, previous)
}

// ApplyTo sets the request's queries and derived metrics from the report, so
// its formulas evaluate over the report's own queries.
func (r *ReportDefinition) ApplyTo(request *SemanticRequest) {
	request.Queries = make([]ModelQuery, 0, len(r.Queries))
	for _, query := range r.Queries {
		request.Queries = append(request.Queries, ModelQuery{ID: query.ID, Model: query.Model, Measures: query.Measures, Dimensions: query.Dimensions, Filters: query.Filters})
	}
	request.Derived = r.Derived
}
