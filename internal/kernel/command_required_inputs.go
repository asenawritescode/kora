package kernel

import (
	"regexp"
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
		if g.Exists != nil {
			addMap(g.Exists.Where)
		}
		if g.NotExists != nil {
			addMap(g.NotExists.Where)
		}
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
		if s.Update != nil {
			add(s.Update.Name)
			addMap(s.Update.Where)
			for _, a := range s.Update.Adjust {
				add(a.Increase, a.Decrease)
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

// missingInput returns the field-level validation error for the first
// required input absent from the payload, or nil.
func (c *CommandResource) missingInput(input map[string]any) *contract.Error {
	for _, name := range c.RequiredInputs() {
		if _, ok := input[name]; !ok {
			err := contract.NewError(contract.CodeValidationFailed, "input "+name+" is required")
			err.Field = name
			return err
		}
	}
	return nil
}
