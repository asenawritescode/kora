package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/recon"
	"github.com/gin-gonic/gin"
)

type businessReconciliationRunRequest struct {
	OwnerModuleID string `json:"owner_module_id"`
	RuleID        string `json:"rule_id"`
	RunID         string `json:"run_id"`
	PreviousRunID string `json:"previous_run_id,omitempty"`
}

type businessFindingDecisionRequest struct {
	FindingID string `json:"finding_id"`
	Reason    string `json:"reason,omitempty"`
}

type businessRuleAuthorization struct {
	execution policy.Execution
	roles     policy.RolePolicy
	scope     contract.BusinessScope
	resource  string
}

func (a businessRuleAuthorization) authorize(scope contract.BusinessScope, actor, operation string) error {
	if !a.execution.Valid() || a.execution.Tenant() != scope.TenantID || actor == "" {
		return errors.New("verified business reconciliation actor is required")
	}
	identity, ok := executionUserID(a.execution)
	if !ok || identity != actor {
		return errors.New("business reconciliation actor does not match verified execution")
	}
	_, authErr := policy.DecideForBusinessScope(a.execution, a.roles, a.resource, operation, scope, scope.Kind == contract.ScopeKindLegalEntity)
	if authErr != nil {
		return authErr
	}
	return nil
}

func (a businessRuleAuthorization) CanRunBusinessReconciliation(_ context.Context, _ contract.TenantID, scope contract.BusinessScope, actor string) error {
	return a.authorize(scope, actor, "report")
}

func (a businessRuleAuthorization) CanReadBusinessRun(_ context.Context, tenant contract.TenantID, _ string, actor string) error {
	if tenant != a.scope.TenantID {
		return errors.New("business reconciliation run tenant mismatch")
	}
	return a.authorize(a.scope, actor, "read")
}

func (a businessRuleAuthorization) CanWaiveBusinessFinding(_ context.Context, tenant contract.TenantID, scope contract.BusinessScope, actor string) error {
	if tenant != scope.TenantID {
		return errors.New("business finding waiver tenant mismatch")
	}
	return a.authorize(scope, actor, "write")
}

func (a businessRuleAuthorization) CanRepairBusinessFinding(_ context.Context, tenant contract.TenantID, scope contract.BusinessScope, actor string) error {
	if tenant != scope.TenantID {
		return errors.New("business finding repair tenant mismatch")
	}
	return a.authorize(scope, actor, "write")
}

func reconRuleFromRegistration(rule plugin.BusinessReconciliationRule, scope contract.BusinessScope) recon.BusinessReconciliationRule {
	return recon.BusinessReconciliationRule{
		OwnerModuleID: rule.OwnerModuleID, ID: rule.ID, Version: rule.Version, Scope: scope,
		SourceQuery: recon.BusinessQueryRef{OwnerModuleID: rule.SourceQueryOwnerModuleID, Name: rule.SourceQueryName, Input: append(json.RawMessage(nil), rule.SourceQueryInput...), PageLimit: rule.SourcePageLimit},
		TargetQuery: recon.BusinessQueryRef{OwnerModuleID: rule.TargetQueryOwnerModuleID, Name: rule.TargetQueryName, Input: append(json.RawMessage(nil), rule.TargetQueryInput...), PageLimit: rule.TargetPageLimit},
		GracePeriod: rule.GracePeriod, MaxRows: rule.MaxRows,
	}
}

func decodeBusinessReconciliationBody(c *gin.Context, target any) error {
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, 65537))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("request must contain one JSON object")
	}
	return nil
}

func (h *Handler) resolveBusinessReconciliationRule(c *gin.Context, owner contract.ModuleID, ruleID string) (policy.Execution, contract.BusinessScope, string, SiteRuntimeService, plugin.BindingPlan, plugin.BusinessReconciliationRule, recon.BusinessReconciliationRule, *plugin.PublishedQueryActorScope, *plugin.PublishedQueryActorScope, error) {
	execution, valid := requestExecution(c)
	if !valid {
		return policy.Execution{}, contract.BusinessScope{}, "", SiteRuntimeService{}, plugin.BindingPlan{}, plugin.BusinessReconciliationRule{}, recon.BusinessReconciliationRule{}, nil, nil, errors.New("verified identity is required")
	}
	scope := businessScopeFromRequest(c, execution)
	actor, ok := executionUserID(execution)
	if !ok {
		return policy.Execution{}, scope, "", SiteRuntimeService{}, plugin.BindingPlan{}, plugin.BusinessReconciliationRule{}, recon.BusinessReconciliationRule{}, nil, nil, errors.New("verified actor is required")
	}
	services := h.runtimeService(c, c.GetString("site_name"))
	if services.BusinessReconciliation == nil || services.ResolveModuleBindings == nil {
		return execution, scope, actor, services, plugin.BindingPlan{}, plugin.BusinessReconciliationRule{}, recon.BusinessReconciliationRule{}, nil, nil, errors.New("business reconciliation is unavailable")
	}
	plan, err := services.ResolveModuleBindings(scope)
	if err != nil {
		return execution, scope, actor, services, plan, plugin.BusinessReconciliationRule{}, recon.BusinessReconciliationRule{}, nil, nil, err
	}
	if len(plan.Blockers) > 0 {
		return execution, scope, actor, services, plan, plugin.BusinessReconciliationRule{}, recon.BusinessReconciliationRule{}, nil, nil, errors.New("module capability blockers prevent business reconciliation")
	}
	var registration plugin.BusinessReconciliationRule
	found := false
	for _, candidate := range plan.BusinessReconciliationRules {
		if candidate.OwnerModuleID == owner && candidate.ID == ruleID {
			registration, found = candidate, true
			break
		}
	}
	if !found {
		return execution, scope, actor, services, plan, registration, recon.BusinessReconciliationRule{}, nil, nil, errors.New("owner reconciliation rule not found")
	}
	sourceQuery, sourceOK := plan.QueryDefinitions[plugin.PublishedQueryKey{Scope: scope, OwnerModuleID: registration.SourceQueryOwnerModuleID, Name: registration.SourceQueryName}]
	targetQuery, targetOK := plan.QueryDefinitions[plugin.PublishedQueryKey{Scope: scope, OwnerModuleID: registration.TargetQueryOwnerModuleID, Name: registration.TargetQueryName}]
	if !sourceOK || !targetOK {
		return execution, scope, actor, services, plan, registration, recon.BusinessReconciliationRule{}, nil, nil, errors.New("owner reconciliation query is unavailable")
	}
	sourceActorScope, sourceAuthErr := authorizePublishedQueryActorScope(execution, h.siteRegistry(c), sourceQuery, scope)
	targetActorScope, targetAuthErr := authorizePublishedQueryActorScope(execution, h.siteRegistry(c), targetQuery, scope)
	if sourceAuthErr != nil || targetAuthErr != nil {
		return execution, scope, actor, services, plan, registration, recon.BusinessReconciliationRule{}, nil, nil, errors.New("published query permission is required")
	}
	rule := reconRuleFromRegistration(registration, scope)
	return execution, scope, actor, services, plan, registration, rule, sourceActorScope, targetActorScope, nil
}

func (h *Handler) HandleBusinessReconciliationRun(c *gin.Context) {
	var request businessReconciliationRunRequest
	if err := decodeBusinessReconciliationBody(c, &request); err != nil || strings.TrimSpace(request.RunID) == "" {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid business reconciliation run request", nil)
		return
	}
	execution, scope, actor, services, plan, registration, rule, sourceScope, targetScope, err := h.resolveBusinessReconciliationRule(c, contract.ModuleID(request.OwnerModuleID), request.RuleID)
	if err != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Business reconciliation rule is unavailable or not granted", nil)
		return
	}
	resource := "module." + string(registration.OwnerModuleID) + ".reconciliation." + registration.ID
	authorizer := businessRuleAuthorization{execution: execution, roles: h.siteRegistry(c), scope: scope, resource: resource}
	if err := authorizer.CanRunBusinessReconciliation(c.Request.Context(), scope.TenantID, scope, actor); err != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Business reconciliation report permission is required", nil)
		return
	}
	service := *services.BusinessReconciliation
	service.ReadPage = func(ctx context.Context, query recon.BusinessQueryRequest) (contract.QueryResult, error) {
		actorScope := sourceScope
		if query.OwnerModuleID == registration.TargetQueryOwnerModuleID && query.Name == registration.TargetQueryName {
			actorScope = targetScope
		}
		if actorScope == nil {
			return plan.ExecutePublishedQuery(ctx, scope, query.OwnerModuleID, query.Name, query.Input, query.Cursor, query.Limit)
		}
		return plan.ExecutePublishedQueryForActor(ctx, scope, query.OwnerModuleID, query.Name, *actorScope, query.Input, query.Cursor, query.Limit)
	}
	var run recon.BusinessRun
	if request.PreviousRunID == "" {
		run, err = service.Run(c.Request.Context(), actor, request.RunID, rule, time.Now().UTC(), authorizer)
	} else {
		run, err = service.Continue(c.Request.Context(), actor, request.RunID, request.PreviousRunID, rule, authorizer, authorizer)
	}
	if err != nil {
		writeError(c, http.StatusConflict, "reconciliation.run_failed", "Business reconciliation run could not be completed", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: run})
}

func (h *Handler) HandleBusinessReconciliationRead(c *gin.Context) {
	handleBusinessReconciliationFindingDecision(c, h, "read")
}

func (h *Handler) HandleBusinessReconciliationWaive(c *gin.Context) {
	handleBusinessReconciliationFindingDecision(c, h, "waive")
}

func (h *Handler) HandleBusinessReconciliationRepair(c *gin.Context) {
	owner := contract.ModuleID(c.Param("owner"))
	ruleID, runID := c.Param("rule"), c.Param("run")
	execution, scope, actor, services, plan, registration, _, _, _, err := h.resolveBusinessReconciliationRule(c, owner, ruleID)
	if err != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Business reconciliation rule is unavailable or not granted", nil)
		return
	}
	var request businessFindingDecisionRequest
	if err := decodeBusinessReconciliationBody(c, &request); err != nil || strings.TrimSpace(request.FindingID) == "" {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "A finding ID is required", nil)
		return
	}
	if plan.LeaseReader == nil {
		writeError(c, http.StatusServiceUnavailable, "reconciliation.repair_unavailable", "Owner repair capability is unavailable", nil)
		return
	}
	lease, found, err := plan.LeaseReader.LoadCapabilityLease(c.Request.Context(), scope.TenantID, registration.RepairLeaseID)
	if err != nil || !found {
		writeError(c, http.StatusServiceUnavailable, "reconciliation.repair_unavailable", "Owner repair capability is unavailable", nil)
		return
	}
	resource := "module." + string(registration.OwnerModuleID) + ".reconciliation." + registration.ID
	authorizer := businessRuleAuthorization{execution: execution, roles: h.siteRegistry(c), scope: scope, resource: resource}
	repairer := businessCapabilityRepairer{plan: plan, execution: execution, scope: scope, registration: registration, lease: lease}
	if err := services.BusinessReconciliation.RepairFinding(c.Request.Context(), scope.TenantID, runID, request.FindingID, actor, authorizer, repairer); err != nil {
		writeError(c, http.StatusConflict, "reconciliation.repair_pending", "Owner repair remains pending reconciliation", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: gin.H{"repaired": true}})
}

type businessCapabilityRepairer struct {
	plan         plugin.BindingPlan
	execution    policy.Execution
	scope        contract.BusinessScope
	registration plugin.BusinessReconciliationRule
	lease        contract.CapabilityLease
}

func (r businessCapabilityRepairer) RepairBusinessObligation(ctx context.Context, expected recon.BusinessObligation, idempotency contract.IdempotencyKey) (string, error) {
	input, err := json.Marshal(expected)
	if err != nil {
		return "", err
	}
	result, err := r.plan.InvokeCapability(ctx, r.execution, r.lease, r.scope, r.registration.RepairCallerModuleID, r.registration.RepairCapability, r.registration.RepairOperation, contract.CommandMeta{IdempotencyKey: idempotency}, input)
	if err != nil {
		return "", err
	}
	if result.Status != "completed" || result.ExecutionID == "" {
		return "", errors.New("owner repair command returned no completed execution evidence")
	}
	return string(result.ExecutionID), nil
}

func handleBusinessReconciliationFindingDecision(c *gin.Context, h *Handler, action string) {
	owner := contract.ModuleID(c.Param("owner"))
	ruleID, runID := c.Param("rule"), c.Param("run")
	execution, scope, actor, services, _, registration, _, sourceScope, targetScope, err := h.resolveBusinessReconciliationRule(c, owner, ruleID)
	if err != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Business reconciliation rule is unavailable or not granted", nil)
		return
	}
	resource := "module." + string(registration.OwnerModuleID) + ".reconciliation." + registration.ID
	authorizer := businessRuleAuthorization{execution: execution, roles: h.siteRegistry(c), scope: scope, resource: resource}
	if action == "read" {
		// Stored findings contain source and target evidence. Until those rows
		// can be re-filtered against the reader's row scope, deny scoped reads.
		if sourceScope != nil || targetScope != nil {
			writeError(c, http.StatusForbidden, "permission.denied", "Business reconciliation evidence requires unfiltered query access", nil)
			return
		}
		run, err := services.BusinessReconciliation.GetRun(c.Request.Context(), scope.TenantID, runID, actor, authorizer)
		if err != nil {
			writeError(c, http.StatusForbidden, "permission.denied", "Business reconciliation run is not available", nil)
			return
		}
		c.JSON(http.StatusOK, Response{Data: run})
		return
	}
	var request businessFindingDecisionRequest
	if err := decodeBusinessReconciliationBody(c, &request); err != nil || strings.TrimSpace(request.FindingID) == "" || strings.TrimSpace(request.Reason) == "" {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "A finding ID and waiver reason are required", nil)
		return
	}
	if err := services.BusinessReconciliation.WaiveFinding(c.Request.Context(), scope.TenantID, runID, request.FindingID, actor, request.Reason, authorizer); err != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Business finding waiver was not accepted", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: gin.H{"waived": true}})
}

type businessFindingSummaryAuthorization struct {
	execution policy.Execution
	roles     policy.RolePolicy
}

func (a businessFindingSummaryAuthorization) CanInspectBusinessFindings(_ context.Context, scope contract.BusinessScope, actor string) error {
	if !a.execution.Valid() || a.execution.Tenant() != scope.TenantID {
		return errors.New("verified tenant execution is required")
	}
	identity := a.execution.Actor()
	subject := strings.TrimSpace(identity.SubjectUserID)
	if subject == "" {
		subject = strings.TrimSpace(identity.PrincipalID)
	}
	if actor == "" || actor != subject {
		return errors.New("business finding summary actor does not match verified execution")
	}
	_, authErr := policy.DecideForBusinessScope(a.execution, a.roles, "module.business_reconciliation.findings", "read", scope, scope.Kind == contract.ScopeKindLegalEntity)
	if authErr != nil {
		return authErr
	}
	return nil
}

// HandleBusinessReconciliationSummary returns only unresolved business
// findings for the caller's exact scope. Delivery failures remain a separate
// inspection surface.
func (h *Handler) HandleBusinessReconciliationSummary(c *gin.Context) {
	execution, valid := requestExecution(c)
	if !valid {
		writeError(c, http.StatusUnauthorized, "auth.unauthenticated", "Verified identity is required", nil)
		return
	}
	scope := businessScopeFromRequest(c, execution)
	actor, ok := executionUserID(execution)
	if !ok {
		writeError(c, http.StatusForbidden, "permission.denied", "Business reconciliation inspection is not granted", nil)
		return
	}
	service := h.runtimeService(c, c.GetString("site_name")).BusinessReconciliation
	if service == nil {
		writeError(c, http.StatusServiceUnavailable, "reconciliation.unavailable", "Business reconciliation inspection is unavailable", nil)
		return
	}
	count, err := service.CountUnresolvedFindings(c.Request.Context(), scope, actor, businessFindingSummaryAuthorization{execution: execution, roles: h.siteRegistry(c)})
	if err != nil {
		if errors.Is(err, recon.ErrBusinessFindingSummaryDenied) {
			writeError(c, http.StatusForbidden, "permission.denied", "Business reconciliation inspection is not granted", nil)
			return
		}
		writeError(c, http.StatusServiceUnavailable, "reconciliation.unavailable", "Business reconciliation inspection is unavailable", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: gin.H{"scope": scope, "unresolved_business_findings": count}})
}
