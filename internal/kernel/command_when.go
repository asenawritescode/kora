package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/expression"
)

// whenRefs rewrites $input and $computed references into identifiers, since
// the expression parser reserves "$".
var whenRefs = strings.NewReplacer("$input.", "input.", "$computed.", "computed.")

// validateWhen compiles the when condition of step i. It accepts only extra
// inputs and compute keys, both known at load, so a typo fails here.
func (c *CommandResource) validateWhen(i int) error {
	step := &c.Steps[i]
	if step.When == "" {
		return nil
	}
	if step.As != "" {
		return fmt.Errorf("command %q: step %d when cannot be combined with as", c.FullName(), i)
	}
	parsed, err := expression.Parse(whenRefs.Replace(step.When), 64, 16)
	if err != nil || parsed == nil || len(parsed.Errors) > 0 {
		return fmt.Errorf("command %q: step %d when is not a valid condition", c.FullName(), i)
	}
	for _, dep := range parsed.Deps {
		root, name, _ := strings.Cut(dep, ".")
		_, isExtra := c.Input.Extra[name]
		if (root == "input" && isExtra) || (root == "computed" && c.hasComputeKey(name)) {
			continue
		}
		return fmt.Errorf("command %q: step %d when reference %q must name an extra input or a compute key", c.FullName(), i, dep)
	}
	step.cond = parsed.AST
	return nil
}

// stepRuns reports whether the step runs. A step without when always runs.
func (s CommandStep) stepRuns(env refEnv) (bool, error) {
	if s.When == "" {
		return true, nil
	}
	if s.cond == nil {
		return false, errors.New("when was not validated at load")
	}
	v, err := expression.NewTreeEvaluator().Eval(context.Background(), s.cond, whenScope{input: env.input, computed: env.computed})
	if err != nil {
		return false, err
	}
	if v.Kind != expression.ValueBool {
		return false, errors.New("condition must be a boolean")
	}
	return v.Bool, nil
}

// whenScope resolves the identifiers validateWhen admitted.
type whenScope struct {
	input, computed map[string]any
}

func (s whenScope) Lookup(path string) (expression.Value, bool) {
	root, name, _ := strings.Cut(path, ".")
	var raw any
	var found bool
	switch root {
	case "input":
		raw, found = s.input[name]
	case "computed":
		raw, found = s.computed[name]
	}
	if !found {
		return expression.NullValue(), false
	}
	switch v := raw.(type) {
	case bool:
		return expression.BoolValue(v), true
	case string:
		return expression.StringValue(v), true
	case float64:
		return expression.NumberValue(v), true
	case int:
		return expression.NumberValue(float64(v)), true
	}
	return expression.NullValue(), false
}

func (whenScope) Aggregate(fn, table, field string) (expression.Value, error) {
	return expression.Value{}, fmt.Errorf("%s(%s.%s) is not allowed in when", fn, table, field)
}

// validateExtraDefault allows a default only on a boolean input that is not required.
func (c *CommandResource) validateExtraDefault(name string, extra CommandExtraInput) error {
	if extra.Default != nil && (extra.Type != "boolean" || extra.Required) {
		return fmt.Errorf("command %q: input.extra.%s default needs a boolean input that is not required", c.Name, name)
	}
	return nil
}

// readInput decodes the payload and fills the defaults of the extra inputs it omits.
func (c *CommandResource) readInput(raw json.RawMessage) (map[string]any, *contract.Error) {
	input, cerr := decodeInput(raw)
	if cerr != nil {
		return nil, cerr
	}
	c.applyInputDefaults(input)
	return input, nil
}

// applyInputDefaults fills each defaulted extra input the payload omits.
func (c *CommandResource) applyInputDefaults(input map[string]any) {
	for name, extra := range c.Input.Extra {
		if _, given := input[name]; !given && extra.Default != nil {
			input[name] = *extra.Default
		}
	}
}
