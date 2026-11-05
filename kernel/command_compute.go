package kernel

import (
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"github.com/asenawritescode/kora/expression"
	"gopkg.in/yaml.v3"
)

// MaxComputeEntries bounds the compute block of one command.
const MaxComputeEntries = 32

var computeKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// ComputeEntry derives one value from the command input before any step runs.
// Exactly one of Sum (with Expr) or Count must be set. Sum and Count name an
// "$input.<list>" and Expr is evaluated per element, with bare names reading
// the element's fields.
type ComputeEntry struct {
	Key   string `yaml:"-"               json:"key"`
	Sum   string `yaml:"sum,omitempty"   json:"sum,omitempty"`
	Count string `yaml:"count,omitempty" json:"count,omitempty"`
	Expr  string `yaml:"expr,omitempty"  json:"expr,omitempty"`

	ast *expression.Expr
}

// ComputeBlock keeps compute entries in declaration order, which is the
// evaluation order.
type ComputeBlock []ComputeEntry

// UnmarshalYAML reads a mapping of key to entry and preserves its order.
func (b *ComputeBlock) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("compute must be a mapping of key to entry")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		var e ComputeEntry
		if err := node.Content[i+1].Decode(&e); err != nil {
			return err
		}
		e.Key = node.Content[i].Value
		*b = append(*b, e)
	}
	return nil
}

func (c *CommandResource) validateCompute() error {
	if len(c.Compute) > MaxComputeEntries {
		return fmt.Errorf("command %q: compute has more than %d entries", c.FullName(), MaxComputeEntries)
	}
	seen := map[string]bool{}
	for i := range c.Compute {
		e := &c.Compute[i]
		if !computeKeyPattern.MatchString(e.Key) || seen[e.Key] {
			return fmt.Errorf("command %q: compute key %q must be a unique lower-case identifier", c.FullName(), e.Key)
		}
		seen[e.Key] = true
		if (e.Sum == "") == (e.Count == "") {
			return fmt.Errorf("command %q: compute %q needs exactly one of sum or count", c.FullName(), e.Key)
		}
		list := e.Sum + e.Count
		if !strings.HasPrefix(list, "$input.") {
			return fmt.Errorf("command %q: compute %q must reference an $input list", c.FullName(), e.Key)
		}
		if e.Count != "" {
			if e.Expr != "" {
				return fmt.Errorf("command %q: compute %q count takes no expr", c.FullName(), e.Key)
			}
			continue
		}
		parsed, err := expression.Parse(e.Expr, 64, 16)
		if err != nil || parsed == nil || len(parsed.Errors) > 0 {
			return fmt.Errorf("command %q: compute %q expr invalid", c.FullName(), e.Key)
		}
		e.ast = parsed.AST
	}
	return nil
}

// evalCompute runs the compute block against the input. Results are exact
// decimal strings, so 3 x 0.1 is "0.3" and never a float artefact.
func evalCompute(block ComputeBlock, input map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(block))
	for _, e := range block {
		list, err := resolveEnvRef(e.Sum+e.Count, refEnv{input: input})
		if err != nil {
			return nil, fmt.Errorf("compute %q: %w", e.Key, err)
		}
		items, ok := list.([]any)
		if !ok {
			return nil, fmt.Errorf("compute %q: %q must be a list", e.Key, e.Sum+e.Count)
		}
		if e.Count != "" {
			out[e.Key] = strconv.Itoa(len(items))
			continue
		}
		total := new(big.Rat)
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("compute %q: list elements must be objects", e.Key)
			}
			v, err := evalExact(e.ast, item)
			if err != nil {
				return nil, fmt.Errorf("compute %q: %w", e.Key, err)
			}
			total.Add(total, v)
		}
		out[e.Key] = formatRat(total)
	}
	return out, nil
}

func evalExact(n *expression.Expr, item map[string]any) (*big.Rat, error) {
	switch n.Kind {
	case expression.ExprLiteral:
		return toRat(n.Value.Number)
	case expression.ExprIdent:
		v, ok := item[n.Name]
		if !ok {
			return nil, fmt.Errorf("value %q is missing", n.Name)
		}
		return toRat(v)
	case expression.ExprBinary:
		l, err := evalExact(n.L, item)
		if err != nil {
			return nil, err
		}
		r, err := evalExact(n.R, item)
		if err != nil {
			return nil, err
		}
		switch n.Op {
		case "+":
			return new(big.Rat).Add(l, r), nil
		case "-":
			return new(big.Rat).Sub(l, r), nil
		case "*":
			return new(big.Rat).Mul(l, r), nil
		case "/":
			if r.Sign() == 0 {
				return nil, fmt.Errorf("division by zero")
			}
			return new(big.Rat).Quo(l, r), nil
		}
	}
	return nil, fmt.Errorf("unsupported expression element")
}

// toRat converts a number or numeric string through its shortest decimal text
// so 0.1 stays exactly one tenth.
func toRat(v any) (*big.Rat, error) {
	var s string
	switch x := v.(type) {
	case float64:
		s = strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		s = strconv.Itoa(x)
	case string:
		s = strings.TrimSpace(x)
	default:
		return nil, fmt.Errorf("value %v is not numeric", v)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("value %q is not numeric", s)
	}
	return r, nil
}

func formatRat(r *big.Rat) string {
	s := r.FloatString(12)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func (c *CommandResource) hasComputeKey(key string) bool {
	for _, e := range c.Compute {
		if e.Key == key {
			return true
		}
	}
	return false
}
