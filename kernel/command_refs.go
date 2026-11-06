package kernel

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
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
	// rows holds stored child rows of guarded records, keyed alias.field.
	rows map[string][]map[string]any
	// computed holds values derived by the command's compute block.
	computed map[string]any
	// now is pinned once per execution; actor is the verified principal.
	now   time.Time
	actor string
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
	case strings.HasPrefix(ref, "$computed."):
		v, found := env.computed[strings.TrimPrefix(ref, "$computed.")]
		if !found {
			return nil, fmt.Errorf("reference %q does not name a computed value", ref)
		}
		return v, nil
	case strings.HasPrefix(ref, "$steps."):
		alias, ok := strings.CutSuffix(strings.TrimPrefix(ref, "$steps."), ".name")
		name, found := env.steps[alias]
		if !ok || !found {
			return nil, fmt.Errorf("reference %q does not name an earlier step", ref)
		}
		return name, nil
	case ref == "$now":
		return env.now.UTC().Format(time.RFC3339Nano), nil
	case ref == "$today":
		return env.now.UTC().Format("2006-01-02"), nil
	case ref == "$actor":
		return env.actor, nil
	default:
		return ref, nil
	}
}

// errRefAbsent marks a ref whose key is missing; step values skip it.
var errRefAbsent = errors.New("reference missing")

func walkRef(ref string, root map[string]any, path string) (any, error) {
	cur := any(root)
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("reference %q path invalid", ref)
		}
		cur, ok = m[part]
		if !ok {
			return nil, fmt.Errorf("reference %q missing in input: %w", ref, errRefAbsent)
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
func expandCommandSteps(steps []CommandStep, env refEnv, rows map[string][]map[string]any) ([]stepInstance, error) {
	out := make([]stepInstance, 0, len(steps))
	for i, step := range steps {
		if step.ForEach == "" {
			out = append(out, stepInstance{index: i, step: step})
			continue
		}
		var list []any
		if key, ok := guardRowsKey(step.ForEach); ok {
			for _, row := range rows[key] {
				list = append(list, row)
			}
		} else {
			value, err := resolveEnvRef(step.ForEach, refEnv{input: env.input})
			if err != nil {
				return nil, fmt.Errorf("step %d: %w", i, err)
			}
			if list, ok = value.([]any); !ok {
				return nil, fmt.Errorf("step %d: for_each %q must resolve to a list", i, step.ForEach)
			}
		}
		want, err := resolveWhere(step.Where, env)
		if err != nil {
			return nil, fmt.Errorf("step %d: where: %w", i, err)
		}
		for _, element := range list {
			item, ok := element.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("step %d: for_each %q elements must be objects", i, step.ForEach)
			}
			if matchesWhere(item, want) {
				out = append(out, stepInstance{index: i, step: step, item: item})
			}
		}
		if len(out) > MaxCommandStepInstances {
			return nil, fmt.Errorf("command expands to more than %d writes", MaxCommandStepInstances)
		}
	}
	return out, nil
}

func resolveWhere(where map[string]string, env refEnv) (map[string]any, error) {
	want := make(map[string]any, len(where))
	for field, ref := range where {
		v, err := resolveEnvRef(ref, refEnv{input: env.input, steps: env.steps, computed: env.computed, now: env.now, actor: env.actor})
		if err != nil {
			return nil, err
		}
		want[field] = v
	}
	return want, nil
}

// matchesWhere reports whether every wanted field equals the element's value.
func matchesWhere(item, want map[string]any) bool {
	for field, v := range want {
		got, ok := item[field]
		if !ok || fmt.Sprint(got) != fmt.Sprint(v) {
			return false
		}
	}
	return true
}

// validateStepReferences checks, at load time, that every reference names a
// root available to its step.
func (c *CommandResource) validateStepReferences() error {
	aliases := map[string]bool{}
	if err := c.validateGuards(aliases); err != nil {
		return err
	}
	guardAliases := map[string]bool{}
	for alias := range aliases {
		guardAliases[alias] = true
	}
	for i, step := range c.Steps {
		if step.ForEach != "" && !strings.HasPrefix(step.ForEach, "$input.") {
			alias, field, ok := strings.Cut(strings.TrimPrefix(step.ForEach, "$steps."), ".")
			if !strings.HasPrefix(step.ForEach, "$steps.") || !ok || field == "name" || strings.Contains(field, ".") || !guardAliases[alias] {
				return fmt.Errorf("command %q: step %d for_each must reference $input or a require alias's table field", c.FullName(), i)
			}
		}
		if len(step.Where) > 0 && step.ForEach == "" {
			return fmt.Errorf("command %q: step %d where requires for_each", c.FullName(), i)
		}
		refs := []string{}
		for _, ref := range step.Where {
			if strings.HasPrefix(ref, "$item.") {
				return fmt.Errorf("command %q: step %d where cannot use %q", c.FullName(), i, ref)
			}
			refs = append(refs, ref)
		}
		var values map[string]string
		if create := step.createSpec(); create != nil {
			values = create.Values
		}
		if step.Update != nil {
			values = step.Update.Values
			refs = append(refs, step.Update.Name)
			for _, ref := range step.Update.Where {
				refs = append(refs, ref)
			}
			for _, change := range step.Update.Adjust {
				refs = append(refs, change.Increase, change.Decrease)
			}
		}
		for _, ref := range values {
			refs = append(refs, ref)
		}
		for _, ref := range refs {
			switch {
			case strings.HasPrefix(ref, "$item.") && step.ForEach == "":
				return fmt.Errorf("command %q: step %d uses %q outside for_each", c.FullName(), i, ref)
			case ref == "$now" || ref == "$today" || ref == "$actor":
			case strings.HasPrefix(ref, "$") && !strings.HasPrefix(ref, "$input.") && !strings.HasPrefix(ref, "$item.") && !strings.HasPrefix(ref, "$computed.") && !strings.HasPrefix(ref, "$steps."):
				return fmt.Errorf("command %q: step %d has unknown reference root in %q", c.FullName(), i, ref)
			case strings.HasPrefix(ref, "$computed."):
				if !c.hasComputeKey(strings.TrimPrefix(ref, "$computed.")) {
					return fmt.Errorf("command %q: step %d reference %q must name a compute key", c.FullName(), i, ref)
				}
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
	return c.validateOutputRefs(aliases)
}

// validateOutputRefs checks output refs resolve statically; aliases holds all
// step aliases, since output is evaluated after every step ran.
func (c *CommandResource) validateOutputRefs(aliases map[string]bool) error {
	for key, ref := range c.Output {
		switch {
		case strings.HasPrefix(ref, "$item."):
			return fmt.Errorf("command %q: output %q cannot use %q", c.FullName(), key, ref)
		case strings.HasPrefix(ref, "$computed."):
			if !c.hasComputeKey(strings.TrimPrefix(ref, "$computed.")) {
				return fmt.Errorf("command %q: output %q reference %q must name a compute key", c.FullName(), key, ref)
			}
		case strings.HasPrefix(ref, "$steps."):
			alias, ok := strings.CutSuffix(strings.TrimPrefix(ref, "$steps."), ".name")
			if !ok || !aliases[alias] {
				return fmt.Errorf("command %q: output %q reference %q must name a step alias", c.FullName(), key, ref)
			}
		}
	}
	return nil
}

// guardRowsKey returns the alias.field key of a $steps for_each reference.
func guardRowsKey(ref string) (string, bool) {
	key, ok := strings.CutPrefix(ref, "$steps.")
	return key, ok
}
