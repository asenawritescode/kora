package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/workflow"
)

func (k *Kernel) workflowStatusPolicy(ctx context.Context, tx *sql.Tx, op Operation, table string) (*workflow.StatusPolicy, bool, error) {
	if k == nil {
		return nil, false, nil
	}
	repository := k.WorkflowStatusPolicies
	if repository == nil && k.TxManager != nil {
		repository = k.TxManager.WorkflowStatusPolicies
	}
	if repository == nil {
		return nil, false, nil
	}
	if _, err := contract.ParseTenantID(string(op.Context.TenantID)); err != nil {
		return nil, false, contract.NewError(contract.CodeUnauthenticated, "workflow policy tenant is unavailable")
	}
	return repository.GetStatusPolicyInTx(ctx, tx, op.Context.TenantID, table)
}

func (k *Kernel) rejectWorkflowStateMutation(ctx context.Context, tx *sql.Tx, op Operation, dt *doctype.DocType, raw json.RawMessage) error {
	if dt == nil || len(raw) == 0 {
		return nil
	}
	statusPolicy, found, err := k.workflowStatusPolicy(ctx, tx, op, dt.Name)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil //nolint:nilerr // a payload that is not a JSON object is rejected by payload validation
	}
	if _, supplied := fields[statusPolicy.StateField]; supplied {
		return fmt.Errorf("workflow-managed status field %q can only be changed through a status action", statusPolicy.StateField)
	}
	return nil
}

func (k *Kernel) rejectWorkflowStateOnCreate(ctx context.Context, tx *sql.Tx, op Operation, dt *doctype.DocType, raw json.RawMessage) error {
	if dt == nil || len(raw) == 0 {
		return nil
	}
	statusPolicy, found, err := k.workflowStatusPolicy(ctx, tx, op, dt.Name)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if statusPolicy.InitialState == "" {
		return fmt.Errorf("workflow-managed status field %q has no configured initial value", statusPolicy.StateField)
	}
	field := dt.GetField(statusPolicy.StateField)
	if field == nil {
		return fmt.Errorf("workflow status field %q is not configured on DocType %s", statusPolicy.StateField, dt.Name)
	}
	initial := statusPolicy.InitialState
	if defaultState := strings.TrimSpace(field.Default); defaultState != "" && defaultState != initial {
		return fmt.Errorf("DocType default for workflow status field %q must match initial value %q", statusPolicy.StateField, initial)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil //nolint:nilerr // a payload that is not a JSON object is rejected by payload validation
	}
	value, supplied := fields[statusPolicy.StateField]
	if !supplied {
		return nil
	}
	var state string
	if err := json.Unmarshal(value, &state); err != nil || state != initial {
		return fmt.Errorf("new records must start in initial workflow status %q; later values require a status action", initial)
	}
	return nil
}

// docChange is a record before and after a mutation.
type docChange struct {
	Before, After *doctype.Document
}

func (k *Kernel) rejectWorkflowStateChange(ctx context.Context, tx *sql.Tx, op Operation, dt *doctype.DocType, change docChange) error {
	before, after := change.Before, change.After
	if dt == nil || before == nil || after == nil {
		return nil
	}
	statusPolicy, found, err := k.workflowStatusPolicy(ctx, tx, op, dt.Name)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if fmt.Sprint(before.Get(statusPolicy.StateField)) != fmt.Sprint(after.Get(statusPolicy.StateField)) {
		return fmt.Errorf("workflow-managed status field %q can only be changed through a status action", statusPolicy.StateField)
	}
	return nil
}

func (k *Kernel) initializeWorkflowStateOnCreate(ctx context.Context, tx *sql.Tx, op Operation, dt *doctype.DocType, doc *doctype.Document) error {
	if dt == nil || doc == nil {
		return nil
	}
	statusPolicy, found, err := k.workflowStatusPolicy(ctx, tx, op, dt.Name)
	if err != nil || !found {
		return err
	}
	if statusPolicy.InitialState == "" {
		return fmt.Errorf("workflow status field %q has no configured initial value", statusPolicy.StateField)
	}
	if dt.GetField(statusPolicy.StateField) == nil {
		return fmt.Errorf("workflow status field %q is not configured on DocType %s", statusPolicy.StateField, dt.Name)
	}
	stateName := strings.TrimSpace(fmt.Sprint(doc.Get(statusPolicy.StateField)))
	if stateName == "<nil>" || stateName == "" {
		doc.Set(statusPolicy.StateField, statusPolicy.InitialState)
	} else if stateName != statusPolicy.InitialState {
		return fmt.Errorf("new records must start in initial workflow status %q; later values require a status action", statusPolicy.InitialState)
	}
	return nil
}
