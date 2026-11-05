package kernel

import (
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
)

// iteratedGuardAlias returns the require alias whose child rows a step loops over.
func iteratedGuardAlias(step CommandStep) (string, bool) {
	key, ok := guardRowsKey(step.ForEach)
	if !ok {
		return "", false
	}
	alias, _, _ := strings.Cut(key, ".")
	return alias, true
}

// guardRecord returns the doctype a require exists guard bound to alias.
func (c *CommandResource) guardRecord(reg *doctype.Registry, alias string) *doctype.DocType {
	for _, guard := range c.Require {
		if guard.Exists != nil && guard.As == alias {
			return reg.Get(guard.Exists.Record)
		}
	}
	return nil
}

// loadGuardRows reads the stored child rows a for_each iterates, inside the
// command transaction after the guards passed and before any step writes.
func (k *Kernel) loadGuardRows(txMgr *orm.TxManager, reg *doctype.Registry, op Operation, cmd *CommandResource, names map[string]string) (map[string][]map[string]any, *contract.Error) {
	rows := map[string][]map[string]any{}
	for _, step := range cmd.Steps {
		alias, ok := iteratedGuardAlias(step)
		key, _ := guardRowsKey(step.ForEach)
		if _, done := rows[key]; !ok || done {
			continue
		}
		field := strings.TrimPrefix(key, alias+".")
		dt := cmd.guardRecord(reg, alias)
		var f *doctype.Field
		if dt != nil {
			f = dt.GetField(field)
		}
		if f == nil || f.Fieldtype != "Table" {
			return nil, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("for_each %q does not name a Table field", step.ForEach))
		}
		childDT := reg.Get(f.Options)
		if childDT == nil {
			return nil, contract.NewError(contract.CodeNotFound, fmt.Sprintf("child doctype %q not found", f.Options))
		}
		doc, err := txMgr.GetDocWithScope(dt, names[alias], scopeForOperation(reg, op, dt.Name, "read"))
		if err != nil {
			return nil, contract.NewError(contract.CodeDependencyUnavailable, fmt.Sprintf("for_each %q: load failed", step.ForEach))
		}
		children, _ := doc.Fields[field].([]*doctype.Document)
		rows[key] = make([]map[string]any, 0, len(children))
		for _, child := range children {
			item := map[string]any{}
			for _, cf := range childDT.NonTableDataFields() {
				item[cf.Fieldname] = child.Fields[cf.Fieldname]
			}
			rows[key] = append(rows[key], item)
		}
	}
	return rows, nil
}
