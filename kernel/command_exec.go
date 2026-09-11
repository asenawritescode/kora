package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
)

// authorizeOp dispatches authorization: built-in commands authorize against
// the payload's doctype; config-defined commands authorize EVERY touched
// record at per-step least privilege — create steps need "create", update
// steps need "write" on their target record (default-deny, spec §51/§108).
func authorizeOp(reg *doctype.Registry, op Operation, def CommandDefinition, dyn *CommandResource) *contract.Error {
	if dyn == nil {
		return authorize(reg, op, def)
	}
	if reg == nil {
		return ErrPermission
	}
	for _, step := range dyn.Steps {
		switch {
		case step.Create != nil:
			if allowed, _ := reg.CanUser(op.Context.Roles, step.Create.Record, "create"); !allowed {
				return ErrPermission
			}
		case step.Update != nil:
			if allowed, _ := reg.CanUser(op.Context.Roles, step.Update.Record, "write"); !allowed {
				return ErrPermission
			}
		}
	}
	return nil
}

type stepOutcome struct {
	Record  string `json:"record"`
	Name    string `json:"name"`
	Created bool   `json:"created"`
}

// execDefinedCommand runs a config-defined command inside ONE SQL transaction:
// receipt claim, per-step writes (each validated against its target record
// schema), emitted events into the outbox, and the audit row — all commit or
// roll back together.
func (k *Kernel) execDefinedCommand(ctx context.Context, siteDB *sql.DB, txMgr *orm.TxManager, reg *doctype.Registry, op Operation, def CommandDefinition, opID string, cmd *CommandResource) (json.RawMessage, *contract.Error) {
	dbTx, err := siteDB.Begin()
	if err != nil {
		return nil, contract.NewError(contract.CodeDependencyUnavailable, "begin transaction: "+err.Error())
	}
	defer dbTx.Rollback()

	input, cerr := decodeInput(op.Payload)
	if cerr != nil {
		return nil, cerr
	}

	user := op.Context.User
	if user == "" {
		user = "system"
	}

	outcomes := make([]stepOutcome, 0, len(cmd.Steps))
	lastRecord, lastName := "", ""

	for i, step := range cmd.Steps {
		switch {
		case step.Create != nil:
			dt := reg.Get(step.Create.Record)
			if dt == nil {
				return nil, contract.NewError(contract.CodeNotFound, fmt.Sprintf("command %q: record %q not found", def.Name, step.Create.Record))
			}
			doc := doctype.NewDocument(dt.Name)
			for field, ref := range step.Create.Values {
				v, rerr := resolveRef(ref, input)
				if rerr != nil {
					return nil, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("step %d: %v", i, rerr))
				}
				doc.Set(field, v)
			}
			if verrs := doctype.ValidateDocument(dt, doc, reg, nil); verrs.HasErrors() {
				return nil, validationContractError(verrs)
			}
			if op.Context.IdempotencyKey != "" && def.IdempotentByKey && len(outcomes) == 0 {
				if cerr := k.claimReceipt(dbTx, op, def, opID, payloadHash(op.Payload)); cerr != nil {
					return nil, cerr
				}
			}
			if err := txMgr.InsertInTx(dbTx, dt, doc, user, user); err != nil {
				return nil, wrapORMError(err)
			}
			doc.IsNew = false
			outcomes = append(outcomes, stepOutcome{Record: dt.Name, Name: doc.Name, Created: true})
			lastRecord, lastName = dt.Name, doc.Name

		case step.Update != nil:
			dt := reg.Get(step.Update.Record)
			if dt == nil {
				return nil, contract.NewError(contract.CodeNotFound, fmt.Sprintf("command %q: record %q not found", def.Name, step.Update.Record))
			}
			targetName, rerr := resolveStringRef(step.Update.Name, input)
			if rerr != nil || targetName == "" {
				return nil, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("step %d: %v", i, rerr))
			}
			oldDoc, err := txMgr.GetDoc(dt, targetName, "")
			if err != nil {
				if err == sql.ErrNoRows || strings.Contains(err.Error(), orm.ErrNotFound.Error()) {
					return nil, contract.NewError(contract.CodeNotFound, fmt.Sprintf("record %q not found", targetName))
				}
				return nil, contract.NewError(contract.CodeDependencyUnavailable, "load document failed")
			}
			if ev := expectedVersionFrom(op); ev != "" {
				prior, perr := readRowVersion(dbTx, dt.TableName(), targetName)
				if perr != nil {
					return nil, contract.NewError(contract.CodeNotFound, "document not found")
				}
				if prior != ev {
					k.writeFailureAudit(siteDB, op, def, opID, ErrStaleVersion.Type)
					return nil, ErrStaleVersion
				}
			}
			doc := cloneDoc(oldDoc)
			for field, ref := range step.Update.Values {
				v, rerr := resolveRef(ref, input)
				if rerr != nil {
					return nil, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("step %d: %v", i, rerr))
				}
				doc.Set(field, v)
			}
			doc.IsNew = false
			if verrs := doctype.ValidateDocument(dt, doc, reg, oldDoc); verrs.HasErrors() {
				return nil, validationContractError(verrs)
			}
			if op.Context.IdempotencyKey != "" && def.IdempotentByKey && len(outcomes) == 0 {
				if cerr := k.claimReceipt(dbTx, op, def, opID, payloadHash(op.Payload)); cerr != nil {
					return nil, cerr
				}
			}
			if err := txMgr.SaveInTx(dbTx, dt, doc, user, "", oldDoc); err != nil {
				return nil, wrapORMError(err)
			}
			outcomes = append(outcomes, stepOutcome{Record: dt.Name, Name: targetName, Created: false})
			lastRecord, lastName = dt.Name, targetName
		}
	}

	// Emit declared events through the outbox INSIDE the transaction so a
	// committed command's events are guaranteed durable (spec §19, §41).
	eventData, _ := json.Marshal(map[string]any{"command": def.Name, "steps": outcomes})
	for _, evtType := range cmd.Emits {
		env := contract.EventEnvelope{
			Type:          evtType,
			Source:        "kora.kernel",
			Site:          op.Context.Site,
			AggregateType: lastRecord,
			AggregateID:   lastName,
			CorrelationID: op.Context.CorrelationID,
			CausationID:   opID,
			Actor:         op.Context.Actor,
			Data:          eventData,
		}
		if k.Outbox == nil {
			break // no provider wired; skip silently only in tests — production wiring always sets Outbox
		}
		if err := k.Outbox.Append(ctx, dbTx, env); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "recording command event failed")
		}
	}

	row := k.buildAuditRow(op, def, opID, lastRecord, lastName, contract.StatusCompleted, "")
	row.Command = def.Name
	if _, aerr := k.writeAudit(dbTx, row); aerr != nil {
		return nil, contract.NewError(contract.CodeInternal, "audit write failed")
	}

	if err := dbTx.Commit(); err != nil {
		return nil, contract.NewError(contract.CodeInternal, "commit failed")
	}

	raw, _ := json.Marshal(map[string]any{"command": def.Name, "steps": outcomes})
	return raw, nil
}

// decodeInput extracts {"data": {...}} from the operation payload.
func decodeInput(raw json.RawMessage) (map[string]any, *contract.Error) {
	var wrapper struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, contract.NewError(contract.CodeValidationFailed, "payload must be {\"data\":{...}}")
	}
	if wrapper.Data == nil {
		wrapper.Data = map[string]any{}
	}
	return wrapper.Data, nil
}

// resolveRef returns literal values as-is and resolves "$input.path" refs.
func resolveRef(ref string, input map[string]any) (any, error) {
	if !strings.HasPrefix(ref, "$input.") {
		return ref, nil
	}
	cur := any(input)
	for _, part := range strings.Split(strings.TrimPrefix(ref, "$input."), ".") {
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

func resolveStringRef(ref string, input map[string]any) (string, error) {
	v, err := resolveRef(ref, input)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("reference %q must resolve to a string", ref)
	}
	return s, nil
}

// readRowVersion reads the canonical version token for optimistic concurrency.
func readRowVersion(dbTx *sql.Tx, table, name string) (string, error) {
	var m sql.NullTime
	if err := dbTx.QueryRow(
		fmt.Sprintf("SELECT modified FROM %s WHERE name = ?", table), name,
	).Scan(&m); err != nil {
		return "", err
	}
	return CanonicalVersion(m.Time), nil
}
