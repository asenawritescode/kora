package kernel

import (
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/contract"
)

var inputKeyRef = regexp.MustCompile(`^\$input\.([A-Za-z_][A-Za-z0-9_]*)`)

// RequiredInputs lists the top-level input keys the command cannot run
// without: a reference in a require guard, a step selector (for_each, where,
// update name or where, adjust, ensure where) or a compute entry fails the
// command when its key is absent. Values written by create, update and ensure
// steps are skipped when absent, so they are not listed (the target record's
// own required fields govern those). Guards waived for some roles
// (unless_role) are not listed, since those roles may omit what they guard.
// requiredMatchRefs returns the references of an exists or not_exists match that
// the command needs: none for a missing or optional match.
func requiredMatchRefs(match *GuardMatch) map[string]string {
	if match == nil || match.Optional {
		return nil
	}
	return match.Where
}

// The published input schema and the executor's field-level error both use it.
func (c *CommandResource) RequiredInputs() []string {
	set := map[string]bool{}
	add := func(refs ...string) {
		for _, ref := range refs {
			if m := inputKeyRef.FindStringSubmatch(strings.TrimSpace(ref)); m != nil {
				set[m[1]] = true
			}
		}
	}
	addMap := func(values map[string]string) {
		for _, ref := range values {
			add(ref)
		}
	}
	for _, g := range c.Require {
		if len(g.UnlessRole) > 0 {
			continue
		}
		if g.Lock != nil {
			add(g.Lock.Name)
		}
		addMap(requiredMatchRefs(g.Exists))
		addMap(requiredMatchRefs(g.NotExists))
		if g.Payment != nil {
			add(g.Payment.ForEach, g.Payment.Currency)
		}
		add(g.Equal...)
	}
	for _, e := range c.Compute {
		add(e.Sum, e.Count)
	}
	for _, s := range c.Steps {
		add(s.ForEach)
		addMap(s.Where)
		if s.Ensure != nil {
			addMap(s.Ensure.Where)
		}
		add(s.reserveOwner())
		if s.Update != nil {
			add(s.Update.Name)
			addMap(s.Update.Where)
			for _, a := range s.Update.Adjust {
				add(a.Increase, a.Decrease, a.By, a.SignFrom)
			}
		}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// inputError returns the field-level validation error for the first required
// input absent from the payload, then for the first enum input whose value is
// not one of its values, or nil.
func (c *CommandResource) inputError(input map[string]any) *contract.Error {
	for _, name := range c.RequiredInputs() {
		if _, ok := input[name]; !ok {
			err := contract.NewError(contract.CodeValidationFailed, "input "+name+" is required")
			err.Field = name
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Input.Enum)) {
		value, present := input[name]
		if !present {
			continue
		}
		if s, ok := value.(string); ok && slices.Contains(c.Input.Enum[name], s) {
			continue
		}
		err := contract.NewError(contract.CodeValidationFailed, "input "+name+" must be one of: "+strings.Join(c.Input.Enum[name], ", "))
		err.Field = name
		return err
	}
	return nil
}
