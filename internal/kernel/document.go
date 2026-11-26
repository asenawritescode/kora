package kernel

import (
	"encoding/json"
	"fmt"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
)

func setDefaultValues(dt *doctype.DocType, doc *doctype.Document) {
	doctype.ApplyDefaultValues(dt, doc)
}

func scopeForOperation(reg *doctype.Registry, op Operation, doctypeName, permission string) policy.RowScope {
	dt := reg.Get(doctypeName)
	decision, err := decideOperationResource(op, reg, dt, permission)
	if err != nil {
		return policy.RowScope{Business: op.Context.BusinessScope}
	}
	actor := op.Context.Execution.Actor()
	userID := actor.SubjectUserID
	if userID == "" {
		userID = actor.PrincipalID
	}
	return policy.RowScope{UserID: userID, Owner: decision.OwnerOnly, AssignedFields: append([]string(nil), decision.AssignedFields...), Business: op.Context.BusinessScope}
}

// cloneDoc copies a loaded document so mutation attempts never alias the
// previously persisted state.
func cloneDoc(d *doctype.Document) *doctype.Document {
	c := doctype.NewDocument(d.DocType)
	for k, v := range d.Fields {
		c.Fields[k] = v
	}
	c.Name = d.Name
	c.DocStatus = d.DocStatus
	c.Revision = d.Revision
	c.IsNew = false
	return c
}

// applyData merges JSON field data into a document, rejecting unknown keys —
// unsupported configuration is never silently discarded.
func applyData(dt *doctype.DocType, doc *doctype.Document, raw json.RawMessage, reg *doctype.Registry, skipReadOnly bool) error {
	var fields map[string]any
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("data is not a JSON object")
	}
	for key, val := range fields {
		f := dt.GetField(key)
		if f == nil {
			return fmt.Errorf("unknown field %q on doctype %q", key, dt.Name)
		}
		if skipReadOnly && f.ReadOnly {
			continue
		}
		if f.Fieldtype == "Table" {
			if err := setChildRows(doc, f, reg, val, skipReadOnly); err != nil {
				return err
			}
			continue
		}
		doc.Set(key, val)
	}
	return nil
}

// setChildRows replaces a table field's rows with the rows sent for it.
func setChildRows(doc *doctype.Document, f *doctype.Field, reg *doctype.Registry, val any, skipReadOnly bool) error {
	children, err := childDocumentsFromData(f.Fieldname, f, reg, val)
	if err != nil {
		return err
	}
	if skipReadOnly {
		restoreReadOnlyChildFields(reg.Get(f.Options), children, doc.GetTable(f.Fieldname))
	}
	doc.Set(f.Fieldname, children)
	return nil
}

// childDocumentsFromData builds the child rows of table field key from its JSON value.
func childDocumentsFromData(key string, f *doctype.Field, reg *doctype.Registry, val any) ([]*doctype.Document, error) {
	rows, ok := val.([]any)
	if !ok {
		return nil, fmt.Errorf("field %q must be an array of child rows", key)
	}
	if reg.Get(f.Options) == nil {
		return nil, fmt.Errorf("child doctype %q not found", f.Options)
	}
	children := make([]*doctype.Document, 0, len(rows))
	for index, value := range rows {
		row, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("field %q row %d must be an object", key, index)
		}
		children = append(children, childDocumentFromData(f.Options, row))
	}
	return children, nil
}

// childDocumentFromData builds one child row. A non-empty string name refers to an
// existing row, which the ORM then updates in place; the name is not a data field.
func childDocumentFromData(doctypeName string, row map[string]any) *doctype.Document {
	child := doctype.NewDocument(doctypeName)
	if name, ok := row["name"].(string); ok && name != "" {
		child.Name = name
		child.IsNew = false
	}
	for childField, childValue := range row {
		if childField != "name" {
			child.Set(childField, childValue)
		}
	}
	return child
}

// restoreReadOnlyChildFields sets the read-only fields of person-sent child rows: a row that
// names a stored row keeps that row's values, and any other row takes the field's default,
// or stays empty. Trusted writers skip this, so their read-only values are kept as sent.
func restoreReadOnlyChildFields(childDT *doctype.DocType, children, stored []*doctype.Document) {
	defaults := doctype.NewDocument(childDT.Name)
	doctype.ApplyDefaultValues(childDT, defaults)
	for _, child := range children {
		previous := storedChildNamed(stored, child.Name)
		for _, field := range childDT.DataFields() {
			if !field.ReadOnly {
				continue
			}
			switch {
			case previous != nil:
				child.Set(field.Fieldname, previous.Get(field.Fieldname))
			case defaults.Get(field.Fieldname) != nil:
				child.Set(field.Fieldname, defaults.Get(field.Fieldname))
			default:
				delete(child.Fields, field.Fieldname)
			}
		}
	}
}

// storedChildNamed returns the stored child row with the given name, or nil for a new row.
func storedChildNamed(stored []*doctype.Document, name string) *doctype.Document {
	if name == "" {
		return nil
	}
	for _, child := range stored {
		if child.Name == name {
			return child
		}
	}
	return nil
}
