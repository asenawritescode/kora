package kernel

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxCommandStepInstances bounds how many writes one command may expand to
// after for_each, so a single request cannot fan out without limit.
const MaxCommandStepInstances = 500

var stepAliasPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// refEnv is what a step value may reference: the command input, the current
// for_each element, and names of records created or updated by earlier steps.
type refEnv struct {
	input map[string]any
	item  map[string]any
	steps map[string]string
}

type stepInstance struct {
	index int
	step  CommandStep
	item  map[string]any
}

// resolveEnvRef returns literals unchanged and resolves $input.path,
// $item.path and $steps.<alias>.name references.
func resolveEnvRef(ref string, env refEnv) (any, error) {
	switch {
	case strings.HasPrefix(ref, "$input."):
		return walkRef(ref, env.input, strings.TrimPrefix(ref, "$input."))
	case strings.HasPrefix(ref, "$item."):
		if env.item == nil {
			return nil, fmt.Errorf("reference %q is only valid inside for_each", ref)
		}
		return walkRef(ref, env.item, strings.TrimPrefix(ref, "$item."))
	case strings.HasPrefix(ref, "$steps."):
		alias, ok := strings.CutSuffix(strings.TrimPrefix(ref, "$steps."), ".name")
		name, found := env.steps[alias]
		if !ok || !found {
			return nil, fmt.Errorf("reference %q does not name an earlier step", ref)
		}
		return name, nil
	default:
		return ref, nil
	}
}

func walkRef(ref string, root map[string]any, path string) (any, error) {
	cur := any(root)
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("reference %q path invalid", ref)
		}
		cur, ok = m[part]
		if !ok {
			return nil, fmt.Errorf("reference %q missing in input", ref)
		}
	}
	return cur, nil
}

func resolveEnvStringRef(ref string, env refEnv) (string, error) {
	v, err := resolveEnvRef(ref, env)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("reference %q must resolve to a string", ref)
	}
	return s, nil
}

// expandCommandSteps turns each for_each step into one instance per element
// of its input list. Elements must be objects.
func expandCommandSteps(steps []CommandStep, input map[string]any) ([]stepInstance, error) {
	out := make([]stepInstance, 0, len(steps))
	for i, step := range steps {
		if step.ForEach == "" {
			out = append(out, stepInstance{index: i, step: step})
			continue
		}
		value, err := resolveEnvRef(step.ForEach, refEnv{input: input})
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i, err)
		}
		list, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("step %d: for_each %q must resolve to a list", i, step.ForEach)
		}
		for _, element := range list {
			item, ok := element.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("step %d: for_each %q elements must be objects", i, step.ForEach)
			}
			out = append(out, stepInstance{index: i, step: step, item: item})
		}
		if len(out) > MaxCommandStepInstances {
			return nil, fmt.Errorf("command expands to more than %d writes", MaxCommandStepInstances)
		}
	}
	return out, nil
}

// validateStepReferences checks, at load time, that every reference names a
// root available to its step.
func (c *CommandResource) validateStepReferences() error {
	aliases := map[string]bool{}
	for i, step := range c.Steps {
		if step.ForEach != "" && !strings.HasPrefix(step.ForEach, "$input.") {
			return fmt.Errorf("command %q: step %d for_each must reference $input", c.FullName(), i)
		}
		refs := []string{}
		var values map[string]string
		if step.Create != nil {
			values = step.Create.Values
		}
		if step.Update != nil {
			values = step.Update.Values
			refs = append(refs, step.Update.Name)
		}
		for _, ref := range values {
			refs = append(refs, ref)
		}
		for _, ref := range refs {
			switch {
			case strings.HasPrefix(ref, "$item.") && step.ForEach == "":
				return fmt.Errorf("command %q: step %d uses %q outside for_each", c.FullName(), i, ref)
			case strings.HasPrefix(ref, "$steps."):
				alias, ok := strings.CutSuffix(strings.TrimPrefix(ref, "$steps."), ".name")
				if !ok || !aliases[alias] {
					return fmt.Errorf("command %q: step %d reference %q must name an earlier step's alias", c.FullName(), i, ref)
				}
			}
		}
		if step.As != "" {
			if step.ForEach != "" {
				return fmt.Errorf("command %q: step %d cannot alias a for_each step", c.FullName(), i)
			}
			if !stepAliasPattern.MatchString(step.As) || aliases[step.As] {
				return fmt.Errorf("command %q: step %d alias %q must be a unique lower-case identifier", c.FullName(), i, step.As)
			}
			aliases[step.As] = true
		}
	}
	return nil
}
