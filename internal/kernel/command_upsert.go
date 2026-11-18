package kernel

import (
	"encoding/json"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
)

// authorizedSteps returns the steps authorization checks: an upsert is checked
// as the create and the update it may run.
func authorizedSteps(steps []CommandStep) []CommandStep {
	out := make([]CommandStep, 0, len(steps))
	for _, s := range steps {
		if s.Upsert == nil {
			out = append(out, s)
			continue
		}
		create, update := s, s
		create.Upsert, update.Upsert = nil, nil
		create.Create = s.createSpec()
		update.Update = s.updateSpec()
		out = append(out, create, update)
	}
	return out
}

// resolveStep returns the step to run now. An upsert resolves to the create or
// the update it performs. An ensure step whose record exists is skipped: skip is
// true and the step binds the existing record.
func (k *Kernel) resolveStep(run txRun, step CommandStep, at stepEnv, stepOp Operation, stepNames map[string]string) (CommandStep, bool, *contract.Error) {
	if step.Upsert != nil {
		resolved, cerr := k.resolveUpsertStep(run, step, at)
		if cerr != nil {
			return step, false, cerr
		}
		step = resolved
	}
	if step.Ensure != nil {
		existing, cerr := ensureExisting(run.Mgr, run.Reg, step.Ensure, at, scopeForOperation(run.Reg, stepOp, step.Ensure.Record, "write")) //nolint:contextcheck // the context travels in txMgr.Context
		if cerr != nil {
			return step, false, cerr
		}
		if existing != "" {
			// Finding a record is not a write: no outcome, and the idempotency
			// receipt is still claimed by the first step that writes.
			bindStepName(stepNames, step.As, existing)
			return step, true, nil
		}
	}
	return step, false, nil
}

// resolveUpsertStep returns the create or the update the upsert performs now: an
// update when a record of the name exists, otherwise a create under the name.
func (k *Kernel) resolveUpsertStep(run txRun, step CommandStep, at stepEnv) (CommandStep, *contract.Error) {
	up := step.Upsert
	dt := run.Reg.Get(up.Record)
	if dt == nil {
		return step, contract.NewError(contract.CodeNotFound, fmt.Sprintf("upsert record %q not found", up.Record))
	}
	name, err := resolveEnvStringRef(up.Name, at.env)
	if err != nil || name == "" {
		return step, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("step %d: upsert name is required", at.index))
	}
	filters, _ := json.Marshal([][]any{{"name", "=", name}})
	_, exists, err := run.Mgr.FirstMatchingNameLocked(dt, string(filters), policy.RowScope{})
	if err != nil {
		return step, contract.NewError(contract.CodeDependencyUnavailable, fmt.Sprintf("step %d: upsert lookup failed", at.index))
	}
	if exists {
		return CommandStep{As: step.As, ForEach: step.ForEach, Update: step.updateSpec()}, nil
	}
	return CommandStep{As: step.As, ForEach: step.ForEach, Create: step.createSpec()}, nil
}

// childValue turns resolved Table rows into child documents for a field of dt,
// so an update saves its child rows the same way a create does.
func childValue(dt *doctype.DocType, field string, v any) any {
	tf := dt.GetField(field)
	if tf == nil || tf.Fieldtype != "Table" {
		return v
	}
	if rows, ok := v.([]any); ok {
		return childDocuments(tf.Options, rows)
	}
	return v
}

// resolveCommandOutput resolves a command's output references.
func resolveCommandOutput(output map[string]string, outEnv refEnv) (map[string]any, *contract.Error) {
	out := make(map[string]any, len(output))
	for key, ref := range output {
		v, rerr := resolveEnvRef(ref, outEnv)
		if rerr != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("output %q: %v", key, rerr))
		}
		out[key] = v
	}
	return out, nil
}
