package kernel

import (
	"fmt"
	"sort"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
)

// stepAlias is the record a require alias or step alias names. step is nil for
// a require alias.
type stepAlias struct {
	record string
	step   *CommandStep
}

// stepAliases maps every alias the command declares to the record it names.
func (c *CommandResource) stepAliases() map[string]stepAlias {
	out := map[string]stepAlias{}
	for _, guard := range c.Require {
		if guard.Exists != nil && guard.As != "" {
			out[guard.As] = stepAlias{record: guard.Exists.Record}
		}
	}
	for i := range c.Steps {
		step := &c.Steps[i]
		if step.As == "" {
			continue
		}
		switch {
		case step.Update != nil:
			out[step.As] = stepAlias{record: step.Update.Record, step: step}
		case step.createSpec() != nil:
			out[step.As] = stepAlias{record: step.createSpec().Record, step: step}
		}
	}
	return out
}

// fieldReadAliases returns, sorted, the aliases whose record fields (anything
// beyond .name) the command reads. Each one needs read access to its record.
func (c *CommandResource) fieldReadAliases() []string {
	var refs []string
	for _, guard := range c.Require {
		if guard.Lock != nil {
			refs = append(refs, guard.Lock.Name)
		}
		refs = append(refs, guard.Equal...)
		for _, match := range []*GuardMatch{guard.Exists, guard.NotExists} {
			if match != nil {
				for _, ref := range match.Where {
					refs = append(refs, ref)
				}
			}
		}
	}
	for _, step := range c.Steps {
		for _, ref := range step.Where {
			refs = append(refs, ref)
		}
		if create := step.createSpec(); create != nil {
			for _, ref := range create.Values {
				refs = append(refs, ref)
			}
		}
		if step.Update != nil {
			refs = append(refs, step.Update.Name)
			for _, ref := range step.Update.Where {
				refs = append(refs, ref)
			}
			for _, ref := range step.Update.Values {
				refs = append(refs, ref)
			}
			for _, change := range step.Update.Adjust {
				refs = append(refs, change.Increase, change.Decrease)
			}
		}
	}
	for _, ref := range c.Output {
		refs = append(refs, ref)
	}
	seen := map[string]bool{}
	var aliases []string
	for _, ref := range refs {
		if alias, field, ok := stepFieldRef(ref); ok && field != "name" && !seen[alias] {
			seen[alias] = true
			aliases = append(aliases, alias)
		}
	}
	sort.Strings(aliases)
	return aliases
}

// stepFieldReader reads one scalar field of the record an alias names. The
// read runs in the command transaction under the same read scope the guards
// use, so it sees the record's current state (including earlier steps' writes)
// and never a row the actor could not read. Missing or Table fields are errors.
func (k *Kernel) stepFieldReader(txMgr *orm.TxManager, reg *doctype.Registry, op Operation, cmd *CommandResource, names map[string]string) func(alias, field string) (any, error) {
	aliases := cmd.stepAliases()
	return func(alias, field string) (any, error) {
		target, ok := aliases[alias]
		dt := reg.Get(target.record)
		if !ok || dt == nil {
			return nil, fmt.Errorf("$steps.%s.%s: alias does not name a record", alias, field)
		}
		readable := false
		for _, candidate := range dt.NonTableDataFields() {
			readable = readable || candidate.Fieldname == field
		}
		if !readable {
			return nil, fmt.Errorf("$steps.%s.%s: %s has no readable scalar field %q", alias, field, dt.Name, field)
		}
		readOp := op
		if target.step != nil {
			scoped, scopeErr := operationForCommandStep(op, cmd, *target.step)
			if scopeErr != nil {
				return nil, fmt.Errorf("$steps.%s.%s: %s", alias, field, scopeErr.Message)
			}
			readOp = scoped
		}
		doc, err := txMgr.GetDocWithScope(dt, names[alias], scopeForOperation(reg, readOp, dt.Name, "read"))
		if err != nil {
			return nil, fmt.Errorf("$steps.%s.%s: %s %q is not readable", alias, field, dt.Name, names[alias])
		}
		return doc.Fields[field], nil
	}
}

// authorizeStepFieldReads requires read access to the record behind every
// alias whose fields the command reads, unless the published operation's own
// authority already covers that record. Reading .name needs no grant.
func (c *CommandResource) authorizeStepFieldReads(reg *doctype.Registry, op Operation, covered func(*doctype.DocType) bool) *contract.Error {
	aliases := c.stepAliases()
	for _, alias := range c.fieldReadAliases() {
		target := aliases[alias]
		dt := reg.Get(target.record)
		if dt == nil {
			return contract.NewError(contract.CodeNotFound, fmt.Sprintf("doctype %q not found", target.record))
		}
		if covered(dt) {
			continue
		}
		readOp := op
		if target.step != nil {
			scoped, scopeErr := operationForCommandStep(op, c, *target.step)
			if scopeErr != nil {
				return scopeErr
			}
			readOp = scoped
		}
		if _, err := decideOperationResource(readOp, reg, dt, "read"); err != nil {
			return err
		}
	}
	return nil
}
