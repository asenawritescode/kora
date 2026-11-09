package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/plugin"
)

func (e *managementModuleLifecycleExecutor) InspectModules(ctx context.Context, tenant contract.TenantID) (contract.ModuleInspection, error) {
	if e == nil || e.tenants == nil || ctx == nil {
		return contract.ModuleInspection{}, fmt.Errorf("module inspection runtime is unavailable")
	}
	handle, err := e.tenants.AcquireByID(ctx, string(tenant))
	if err != nil {
		return contract.ModuleInspection{}, err
	}
	defer handle.Release()
	runtime := handle.Snapshot()
	if runtime == nil || runtime.DB == nil || runtime.Activation.Tenant != tenant {
		return contract.ModuleInspection{}, fmt.Errorf("module inspection tenant runtime is inconsistent")
	}
	dialect := db.Resolve(runtime.DBType)
	if dialect == nil {
		return contract.ModuleInspection{}, fmt.Errorf("module inspection tenant dialect is unsupported")
	}
	snapshot, err := configstore.NewModuleStore(runtime.DB, dialect).Load(tenant)
	if err != nil {
		return contract.ModuleInspection{}, err
	}
	inspection := contract.ModuleInspection{
		ConfigRevision: snapshot.Revision,
		Installations:  append([]contract.ModuleInstallation(nil), snapshot.Installations...),
		ResourceOwners: append([]contract.ResourceOwnership(nil), snapshot.Ownership...),
		Bindings:       []contract.BindingInspection{}, CapabilityAvailability: []contract.CapabilityInspection{},
		BlockingReasons: []contract.ModuleBlocker{},
	}

	scopes := map[contract.BusinessScope]struct{}{{TenantID: tenant, Kind: contract.ScopeKindTenant}: {}}
	for _, selection := range snapshot.ProviderSelections {
		scopes[selection.Key.Scope] = struct{}{}
	}
	orderedScopes := make([]contract.BusinessScope, 0, len(scopes))
	for scope := range scopes {
		orderedScopes = append(orderedScopes, scope)
	}
	sort.Slice(orderedScopes, func(i, j int) bool {
		if orderedScopes[i].Kind != orderedScopes[j].Kind {
			return orderedScopes[i].Kind < orderedScopes[j].Kind
		}
		return orderedScopes[i].LegalEntityID < orderedScopes[j].LegalEntityID
	})
	bindings := map[string]contract.BindingInspection{}
	availability := map[string]contract.CapabilityInspection{}
	blockers := map[string]contract.ModuleBlocker{}
	needsBindingPlan := false
	for _, module := range snapshot.Modules {
		needsBindingPlan = needsBindingPlan || len(module.Requires) > 0
	}
	for _, scope := range orderedScopes {
		if !needsBindingPlan {
			break
		}
		var plan plugin.BindingPlan
		if runtime.Services.ResolveModuleBindings == nil {
			err = fmt.Errorf("module binding resolver is unavailable")
		} else {
			plan, err = runtime.Services.ResolveModuleBindings(scope)
		}
		if err != nil {
			key := "binding_plan_unavailable"
			blockers[key] = contract.ModuleBlocker{Code: key, Count: 1}
			for _, module := range snapshot.Modules {
				for _, requirement := range module.Requires {
					name := string(module.ID) + "/" + string(requirement.Name)
					state := "blocked"
					if !requirement.Required {
						state = "optional_missing"
					}
					setCapabilityInspection(availability, name, contract.CapabilityInspection{Name: requirement.Name, State: state, ReasonCode: key})
				}
			}
			continue
		}
		for _, diagnostic := range plan.Blockers {
			key := diagnostic.Code + "/" + string(diagnostic.Key.ConsumerModuleID) + "/" + string(diagnostic.Key.CapabilityName)
			if existing, ok := blockers[key]; ok {
				existing.Count = addBoundedCount(existing.Count, 1)
				blockers[key] = existing
			} else {
				blockers[key] = contract.ModuleBlocker{Code: diagnostic.Code, Count: 1}
			}
		}
		for _, binding := range plan.Bindings {
			key := string(binding.Key.ConsumerModuleID) + "/" + string(binding.Key.CapabilityName)
			bindings[key+"/"+binding.ProviderID] = contract.BindingInspection{ConsumerModuleID: binding.Key.ConsumerModuleID, CapabilityName: binding.Key.CapabilityName, ProviderID: binding.ProviderID, BindingRevision: binding.Revision}
			setCapabilityInspection(availability, key, contract.CapabilityInspection{Name: binding.Key.CapabilityName, State: "available"})
		}
		for _, diagnostic := range append(append([]plugin.BindingDiagnostic(nil), plan.Blockers...), plan.Warnings...) {
			key := string(diagnostic.Key.ConsumerModuleID) + "/" + string(diagnostic.Key.CapabilityName)
			state := "blocked"
			for _, module := range snapshot.Modules {
				if module.ID == diagnostic.Key.ConsumerModuleID {
					for _, requirement := range module.Requires {
						if requirement.Name == diagnostic.Key.CapabilityName && !requirement.Required {
							state = "optional_missing"
						}
					}
				}
			}
			setCapabilityInspection(availability, key, contract.CapabilityInspection{Name: diagnostic.Key.CapabilityName, State: state, ReasonCode: diagnostic.Code})
		}
	}
	for _, value := range bindings {
		inspection.Bindings = append(inspection.Bindings, value)
	}
	for _, value := range availability {
		inspection.CapabilityAvailability = append(inspection.CapabilityAvailability, value)
	}
	for _, value := range blockers {
		inspection.BlockingReasons = append(inspection.BlockingReasons, value)
	}
	sort.Slice(inspection.Bindings, func(i, j int) bool {
		if inspection.Bindings[i].ConsumerModuleID != inspection.Bindings[j].ConsumerModuleID {
			return inspection.Bindings[i].ConsumerModuleID < inspection.Bindings[j].ConsumerModuleID
		}
		return inspection.Bindings[i].CapabilityName < inspection.Bindings[j].CapabilityName
	})
	sort.Slice(inspection.CapabilityAvailability, func(i, j int) bool {
		if inspection.CapabilityAvailability[i].Name != inspection.CapabilityAvailability[j].Name {
			return inspection.CapabilityAvailability[i].Name < inspection.CapabilityAvailability[j].Name
		}
		return inspection.CapabilityAvailability[i].State < inspection.CapabilityAvailability[j].State
	})
	sort.Slice(inspection.BlockingReasons, func(i, j int) bool { return inspection.BlockingReasons[i].Code < inspection.BlockingReasons[j].Code })
	sort.Slice(inspection.Installations, func(i, j int) bool {
		return inspection.Installations[i].ModuleID < inspection.Installations[j].ModuleID
	})
	sort.Slice(inspection.ResourceOwners, func(i, j int) bool {
		if inspection.ResourceOwners[i].OwnerModuleID != inspection.ResourceOwners[j].OwnerModuleID {
			return inspection.ResourceOwners[i].OwnerModuleID < inspection.ResourceOwners[j].OwnerModuleID
		}
		return inspection.ResourceOwners[i].Resource.String() < inspection.ResourceOwners[j].Resource.String()
	})

	for _, installation := range snapshot.Installations {
		for _, inspector := range e.moduleInspectors(installation.ModuleID, runtime.Services) {
			values, inspectErr := inspector.InspectOpenWork(ctx, tenant, installation.ModuleID, 100)
			if inspectErr != nil {
				slog.ErrorContext(ctx, "tenant module open-work inspection failed", "tenant_id", tenant, "module_id", installation.ModuleID, "error", inspectErr)
				return contract.ModuleInspection{}, fmt.Errorf("inspect open work for module %s: %w", installation.ModuleID, inspectErr)
			}
			for _, item := range values {
				inspection.BlockingReasons = appendModuleBlocker(inspection.BlockingReasons, item.Code, item.Count)
				switch item.Code {
				case "workflow_instance_open":
					inspection.PendingWork.WorkflowCount = addBoundedCount(inspection.PendingWork.WorkflowCount, item.Count)
				case "external_outcome_unknown":
					inspection.PendingWork.UnknownOutcomeCount = addBoundedCount(inspection.PendingWork.UnknownOutcomeCount, item.Count)
				case "event_delivery_open", "event_delivery_quarantined":
					inspection.PendingWork.DeliveryCount = addBoundedCount(inspection.PendingWork.DeliveryCount, item.Count)
				case "scheduled_execution_open":
					inspection.PendingWork.ScheduledCount = addBoundedCount(inspection.PendingWork.ScheduledCount, item.Count)
				case "domain_obligation_open", "domain_reconciliation_incomplete":
					inspection.PendingWork.OpenObligationCount = addBoundedCount(inspection.PendingWork.OpenObligationCount, item.Count)
				}
			}
		}
	}
	return inspection, nil
}

func setCapabilityInspection(values map[string]contract.CapabilityInspection, key string, candidate contract.CapabilityInspection) {
	prior, exists := values[key]
	if !exists {
		values[key] = candidate
		return
	}
	if prior.State != candidate.State || prior.ReasonCode != candidate.ReasonCode {
		values[key] = contract.CapabilityInspection{Name: candidate.Name, State: "mixed", ReasonCode: "scope_specific_availability"}
	}
}

func appendModuleBlocker(values []contract.ModuleBlocker, code string, count uint32) []contract.ModuleBlocker {
	for i := range values {
		if values[i].Code == code {
			values[i].Count = addBoundedCount(values[i].Count, count)
			return values
		}
	}
	return append(values, contract.ModuleBlocker{Code: code, Count: count})
}

func addBoundedCount(a, b uint32) uint32 {
	if ^uint32(0)-a < b {
		return ^uint32(0)
	}
	return a + b
}

func (e *managementModuleLifecycleExecutor) InspectExecution(ctx context.Context, tenant contract.TenantID, executionID string) (contract.ExecutionInspection, error) {
	if e == nil || e.tenants == nil || ctx == nil || executionID == "" {
		return contract.ExecutionInspection{}, fmt.Errorf("execution inspection identity is invalid")
	}
	handle, err := e.tenants.AcquireByID(ctx, string(tenant))
	if err != nil {
		return contract.ExecutionInspection{}, err
	}
	defer handle.Release()
	runtime := handle.Snapshot()
	if runtime == nil || runtime.DB == nil || runtime.Activation.Tenant != tenant {
		return contract.ExecutionInspection{}, fmt.Errorf("execution inspection tenant runtime is inconsistent")
	}
	dialect := db.Resolve(runtime.DBType)
	if dialect == nil {
		return contract.ExecutionInspection{}, fmt.Errorf("execution inspection tenant dialect is unsupported")
	}
	var raw sql.NullString
	var state, instanceID, stepID string
	err = runtime.DB.QueryRowContext(ctx, db.Rebind(dialect, `SELECT pinned_dispatch_json,status,instance_id,step_id FROM _kora_workflow_attempt WHERE tenant_id=? AND execution_id=? ORDER BY attempt DESC LIMIT 1`), string(tenant), executionID).Scan(&raw, &state, &instanceID, &stepID)
	if err != nil {
		return contract.ExecutionInspection{}, err
	}
	if !raw.Valid {
		return contract.ExecutionInspection{}, sql.ErrNoRows
	}
	var stored struct {
		Dispatch contract.PinnedDispatch `json:"dispatch"`
	}
	if err := json.Unmarshal([]byte(raw.String), &stored); err != nil || stored.Dispatch.Validate() != nil || stored.Dispatch.Scope.TenantID != tenant || stored.Dispatch.ExecutionID != executionID {
		return contract.ExecutionInspection{}, fmt.Errorf("persisted execution inspection evidence is invalid")
	}
	reconciliation := "not_required"
	if state == "outcome_unknown" {
		reconciliation = "required"
	} else if state == "failed" {
		reconciliation = "retry_or_review"
	} else if state == "running" {
		reconciliation = "pending"
	} else if state != "succeeded" {
		reconciliation = "unknown"
	}
	return contract.ExecutionInspection{
		ExecutionID: executionID, CallerModuleID: stored.Dispatch.CallerModuleID, OwnerModuleID: stored.Dispatch.ProviderModuleID,
		ProviderID: stored.Dispatch.ProviderID, BindingRevision: stored.Dispatch.BindingRevision, Contract: stored.Dispatch.Operation,
		CorrelationID: instanceID + "/" + stepID, DeliveryState: state, ReconciliationState: reconciliation,
	}, nil
}
