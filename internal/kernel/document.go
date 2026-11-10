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
			rows, ok := val.([]any)
			if !ok {
				return fmt.Errorf("field %q must be an array of child rows", key)
			}
			childDT := reg.Get(f.Options)
			if childDT == nil {
				return fmt.Errorf("child doctype %q not found", f.Options)
			}
			children := make([]*doctype.Document, 0, len(rows))
			for index, value := range rows {
				row, ok := value.(map[string]any)
				if !ok {
					return fmt.Errorf("field %q row %d must be an object", key, index)
				}
				child := doctype.NewDocument(f.Options)
				for childField, childValue := range row {
					child.Set(childField, childValue)
				}
				children = append(children, child)
			}
			doc.Set(key, children)
			continue
		}
		doc.Set(key, val)
	}
	return nil
}
