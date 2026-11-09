package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/management"
	"github.com/gin-gonic/gin"
)

const managementBodyLimit = 1 << 20

type ManagementOperationService interface {
	Accept(context.Context, contract.PrincipalID, any) (management.Operation, error)
	AcceptForTenant(context.Context, contract.PrincipalID, contract.TenantID, any) (management.Operation, error)
	Get(context.Context, contract.PrincipalID, contract.OperationID) (management.Operation, error)
	ReconcileTenantState(context.Context, contract.PrincipalID, contract.OperationID, contract.ReconcileTenantStateOperation) (management.Operation, error)
	ReconcileTenantProvision(context.Context, contract.PrincipalID, contract.OperationID, contract.ReconcileTenantProvisionOperation) (management.Operation, error)
	ReconcileTenantPackage(context.Context, contract.PrincipalID, contract.OperationID, contract.ReconcileTenantPackageOperation) (management.Operation, error)
	ReconcileTenantDelete(context.Context, contract.PrincipalID, contract.OperationID, contract.ReconcileTenantDeleteOperation) (management.Operation, error)
	ReconcileTenantInvitation(context.Context, contract.PrincipalID, contract.OperationID, contract.ReconcileTenantInvitationOperation) (management.Operation, error)
}

type ManagementInvitationReconciler interface {
	ReconcileInvitation(context.Context, management.Operation, contract.ReconcileTenantInvitationOperation) error
}

type ManagementModuleLifecyclePlanner interface {
	Plan(context.Context, contract.TenantID, contract.PlanModuleChange) (contract.ModuleChangePlan, error)
}

// ManagementTenantMemberPlanner answers the read-only tenant member check. It
// has no durable effect and does not create an operation.
type ManagementTenantMemberPlanner interface {
	PlanMember(context.Context, contract.TenantID, contract.PlanTenantMember) (contract.TenantMemberPlan, error)
}

type ManagementModuleInspector interface {
	InspectModules(context.Context, contract.TenantID) (contract.ModuleInspection, error)
	InspectExecution(context.Context, contract.TenantID, string) (contract.ExecutionInspection, error)
}

type ManagementCapability = contract.ManagementCapability

const (
	ManagementProvisionCapability   = contract.ManagementCapabilityTenantProvision
	ManagementPackageCapability     = contract.ManagementCapabilityTenantPackageApply
	ManagementPackageRecovery       = contract.ManagementCapabilityTenantPackageRetry
	ManagementStateCapability       = contract.ManagementCapabilityTenantStateSet
	ManagementStateRecovery         = contract.ManagementCapabilityTenantStateRetry
	ManagementProvisionRecovery     = contract.ManagementCapabilityTenantProvisionRetry
	ManagementDeleteCapability      = contract.ManagementCapabilityTenantDelete
	ManagementDeleteRecovery        = contract.ManagementCapabilityTenantDeleteRetry
	ManagementInvitationCapability  = contract.ManagementCapabilityTenantInvitation
	ManagementInvitationRecovery    = contract.ManagementCapabilityTenantInvitationRetry
	ManagementModuleLifecycle       = contract.ManagementCapabilityModuleLifecycle
	ManagementMemberRevoke          = contract.ManagementCapabilityTenantMemberRevoke
	ManagementOperationRead         = contract.ManagementCapabilityOperationRead
	ManagementMemberPlanCapability  = contract.ManagementCapabilityTenantMemberPlan
	ManagementMemberGrantCapability = contract.ManagementCapabilityTenantMemberGrant
)

// ManagementOperationRoutes installs only the command routes whose durable
// executor and verified service grants are present in Engine composition.
type ManagementOperationRoutes struct {
	Service                ManagementOperationService
	Caller                 contract.PrincipalID
	Capabilities           map[ManagementCapability]bool
	WorkerReady            bool
	InvitationReconciler   ManagementInvitationReconciler
	ModuleLifecyclePlanner ManagementModuleLifecyclePlanner
	ModuleInspector        ManagementModuleInspector
	MemberPlanner          ManagementTenantMemberPlanner
}

func (h *ManagementInspectHandler) RegisterOperations(router gin.IRouter, config ManagementOperationRoutes) error {
	if config.Service == nil {
		return errors.New("durable management operation service is required")
	}
	if !config.WorkerReady {
		return errors.New("durable management operation worker is not ready")
	}
	if _, err := contract.ParsePrincipalID(string(config.Caller)); err != nil {
		return errors.New("verified management caller identity is required")
	}
	stateEnabled := config.Capabilities[ManagementStateCapability]
	stateRecoveryEnabled := config.Capabilities[ManagementStateRecovery]
	provisionRecoveryEnabled := config.Capabilities[ManagementProvisionRecovery]
	packageRecoveryEnabled := config.Capabilities[ManagementPackageRecovery]
	provisionEnabled := config.Capabilities[ManagementProvisionCapability]
	packageEnabled := config.Capabilities[ManagementPackageCapability]
	deleteEnabled := config.Capabilities[ManagementDeleteCapability]
	deleteRecoveryEnabled := config.Capabilities[ManagementDeleteRecovery]
	invitationEnabled := config.Capabilities[ManagementInvitationCapability]
	moduleLifecycleEnabled := config.Capabilities[ManagementModuleLifecycle]
	memberRevokeEnabled := config.Capabilities[ManagementMemberRevoke]
	invitationRecoveryEnabled := config.Capabilities[ManagementInvitationRecovery]
	memberPlanEnabled := config.Capabilities[ManagementMemberPlanCapability]
	memberGrantEnabled := config.Capabilities[ManagementMemberGrantCapability]
	readEnabled := config.Capabilities[ManagementOperationRead]
	if !stateEnabled && !stateRecoveryEnabled && !provisionEnabled && !provisionRecoveryEnabled && !packageEnabled && !packageRecoveryEnabled && !deleteEnabled && !deleteRecoveryEnabled && !invitationEnabled && !invitationRecoveryEnabled && !moduleLifecycleEnabled && !memberRevokeEnabled && !memberPlanEnabled && !memberGrantEnabled {
		return errors.New("management operation route requires at least one operation capability")
	}
	if !readEnabled {
		return errors.New("management operation routes require operation.read")
	}
	if stateRecoveryEnabled && !stateEnabled {
		return errors.New("tenant-state reconciliation requires tenant.state.set")
	}
	if provisionRecoveryEnabled && !provisionEnabled {
		return errors.New("tenant provisioning reconciliation requires tenant.provision")
	}
	if packageRecoveryEnabled && !packageEnabled {
		return errors.New("package reconciliation requires tenant.package.apply")
	}
	if deleteRecoveryEnabled && !deleteEnabled {
		return errors.New("tenant deletion reconciliation requires tenant.delete")
	}
	if invitationRecoveryEnabled && (!invitationEnabled || config.InvitationReconciler == nil) {
		return errors.New("tenant invitation reconciliation requires invitation delivery and a reconciler")
	}
	if moduleLifecycleEnabled && config.ModuleLifecyclePlanner == nil {
		return errors.New("module lifecycle capability requires a planner")
	}
	if moduleLifecycleEnabled && config.ModuleInspector == nil {
		return errors.New("module lifecycle capability requires a tenant module inspector")
	}
	if memberPlanEnabled && config.MemberPlanner == nil {
		return errors.New("tenant member planning requires a planner")
	}
	group := router.Group("/api/management/v2")
	group.Use(h.authenticate)
	group.GET("/capabilities", func(c *gin.Context) {
		capabilities := make([]contract.ManagementCapability, 0, len(config.Capabilities))
		for capability, enabled := range config.Capabilities {
			if enabled {
				capabilities = append(capabilities, capability)
			}
		}
		sort.Slice(capabilities, func(i, j int) bool { return capabilities[i] < capabilities[j] })
		result := contract.ManagementCapabilitiesV2{Version: 2, Capabilities: capabilities, EngineVersion: BinaryVersion}
		if err := contract.ValidateManagementCapabilitiesV2(result); err != nil {
			writeManagementError(c, http.StatusInternalServerError, contract.CodeInternal, "Engine management capabilities are invalid")
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": result})
	})
	operations := &managementOperationHandler{
		service:                   config.Service,
		caller:                    config.Caller,
		packageRecoveryEnabled:    packageRecoveryEnabled,
		provisionRecoveryEnabled:  provisionRecoveryEnabled,
		deleteRecoveryEnabled:     deleteRecoveryEnabled,
		invitationRecoveryEnabled: invitationRecoveryEnabled,
		invitationReconciler:      config.InvitationReconciler,
		moduleLifecyclePlanner:    config.ModuleLifecyclePlanner,
		moduleInspector:           config.ModuleInspector,
		memberPlanner:             config.MemberPlanner,
	}
	if provisionEnabled {
		group.POST("/tenants", operations.provision)
	}
	if packageEnabled {
		group.POST("/tenants/:id/packages", operations.applyPackage)
	}
	if stateEnabled {
		group.POST("/tenants/:id/state", operations.setState)
	}
	if stateRecoveryEnabled {
		group.POST("/operations/:id/reconcile-tenant-state", operations.reconcileTenantState)
	}
	if provisionRecoveryEnabled {
		group.POST("/operations/:id/reconcile-tenant-provision", operations.reconcileTenantProvision)
	}
	if packageRecoveryEnabled {
		group.POST("/operations/:id/reconcile-tenant-package", operations.reconcileTenantPackage)
	}
	if deleteEnabled {
		group.POST("/tenants/:id/delete", operations.deleteTenant)
	}
	if deleteRecoveryEnabled {
		group.POST("/operations/:id/reconcile-tenant-delete", operations.reconcileTenantDelete)
	}
	if invitationEnabled {
		group.POST("/tenants/:id/invitation", operations.requestTenantInvitation)
	}
	if moduleLifecycleEnabled {
		group.POST("/tenants/:id/modules/plan", operations.planModuleChange)
		group.POST("/tenants/:id/modules/apply", operations.applyModuleChange)
		group.GET("/tenants/:id/modules/inspection", operations.inspectModules)
		group.GET("/tenants/:id/executions/:execution", operations.inspectExecution)
	}
	if memberRevokeEnabled {
		group.POST("/tenants/:id/members/:subject/revoke", operations.revokeTenantMember)
	}
	if memberPlanEnabled {
		group.POST("/tenants/:id/members/plan", operations.planTenantMember)
	}
	if memberGrantEnabled {
		group.POST("/tenants/:id/members", operations.grantTenantMember)
	}
	if invitationRecoveryEnabled {
		group.POST("/operations/:id/reconcile-tenant-invitation", operations.reconcileTenantInvitation)
	}
	group.GET("/operations/:id", operations.get)
	return nil
}

type managementOperationHandler struct {
	service                   ManagementOperationService
	caller                    contract.PrincipalID
	packageRecoveryEnabled    bool
	provisionRecoveryEnabled  bool
	deleteRecoveryEnabled     bool
	invitationRecoveryEnabled bool
	invitationReconciler      ManagementInvitationReconciler
	moduleLifecyclePlanner    ManagementModuleLifecyclePlanner
	moduleInspector           ManagementModuleInspector
	memberPlanner             ManagementTenantMemberPlanner
}

func (h *managementOperationHandler) planTenantMember(c *gin.Context) {
	tenant, ok := managementTenant(c)
	if !ok {
		return
	}
	var request contract.PlanTenantMember
	if !decodeManagementBody(c, &request) || !managementKeyMatches(c, request.Meta.IdempotencyKey) {
		return
	}
	if err := contract.ValidatePlanTenantMember(request); err != nil {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, err.Error())
		return
	}
	plan, err := h.memberPlanner.PlanMember(c.Request.Context(), tenant, request)
	if err != nil {
		writeManagementOperationError(c, err)
		return
	}
	if err := contract.ValidateTenantMemberPlan(plan); err != nil {
		writeManagementError(c, http.StatusInternalServerError, contract.CodeInternal, "tenant member plan is invalid")
		return
	}
	c.JSON(http.StatusOK, Response{Data: plan})
}

func (h *managementOperationHandler) grantTenantMember(c *gin.Context) {
	tenant, ok := managementTenant(c)
	if !ok {
		return
	}
	var command contract.GrantTenantMember
	if !decodeManagementBody(c, &command) || !managementKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	operation, err := h.service.AcceptForTenant(c.Request.Context(), h.caller, tenant, command)
	if err != nil {
		writeManagementOperationError(c, err)
		return
	}
	writeManagedOperation(c, http.StatusAccepted, operation.View())
}

func (h *managementOperationHandler) inspectModules(c *gin.Context) {
	tenant, ok := managementTenant(c)
	if !ok {
		return
	}
	inspection, err := h.moduleInspector.InspectModules(c.Request.Context(), tenant)
	if err != nil {
		writeManagementError(c, http.StatusServiceUnavailable, contract.CodeDependencyUnavailable, "tenant module inspection is unavailable")
		return
	}
	c.JSON(http.StatusOK, Response{Data: inspection})
}

func (h *managementOperationHandler) inspectExecution(c *gin.Context) {
	tenant, ok := managementTenant(c)
	if !ok {
		return
	}
	executionID, err := contract.ParseOperationID(strings.TrimSpace(c.Param("execution")))
	if err != nil {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "valid execution ID is required")
		return
	}
	inspection, err := h.moduleInspector.InspectExecution(c.Request.Context(), tenant, string(executionID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeManagementError(c, http.StatusNotFound, contract.CodeNotFound, "tenant execution inspection is unavailable")
		} else {
			writeManagementError(c, http.StatusServiceUnavailable, contract.CodeDependencyUnavailable, "tenant execution inspection is unavailable")
		}
		return
	}
	c.JSON(http.StatusOK, Response{Data: inspection})
}

func (h *managementOperationHandler) provision(c *gin.Context) {
	var command contract.ProvisionTenant
	if !decodeManagementBody(c, &command) || !managementKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	if command.PackageDigest != "" {
		writeManagementError(c, http.StatusNotImplemented, contract.CodeUnsupportedVersion, "package-bearing provisioning is not enabled; apply the package as a separate durable tenant operation")
		return
	}
	operation, err := h.service.Accept(c.Request.Context(), h.caller, command)
	if err != nil {
		writeManagementOperationError(c, err)
		return
	}
	writeManagedOperation(c, http.StatusAccepted, operation.View())
}

func (h *managementOperationHandler) applyPackage(c *gin.Context) {
	tenant, ok := managementTenant(c)
	if !ok {
		return
	}
	var command contract.ApplyPackage
	if !decodeManagementBody(c, &command) {
		return
	}
	if command.Tenant != tenant {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "route tenant does not match package command tenant")
		return
	}
	if !managementKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	operation, err := h.service.Accept(c.Request.Context(), h.caller, command)
	if err != nil {
		writeManagementOperationError(c, err)
		return
	}
	writeManagedOperation(c, http.StatusAccepted, operation.View())
}

func (h *managementOperationHandler) setState(c *gin.Context) {
	tenant, ok := managementTenant(c)
	if !ok {
		return
	}
	var command contract.SetTenantState
	if !decodeManagementBody(c, &command) || !managementKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	operation, err := h.service.AcceptForTenant(c.Request.Context(), h.caller, tenant, command)
	if err != nil {
		writeManagementOperationError(c, err)
		return
	}
	writeManagedOperation(c, http.StatusAccepted, operation.View())
}

func (h *managementOperationHandler) deleteTenant(c *gin.Context) {
	tenant, ok := managementTenant(c)
	if !ok {
		return
	}
	var command contract.DeleteTenant
	if !decodeManagementBody(c, &command) || !managementKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	operation, err := h.service.AcceptForTenant(c.Request.Context(), h.caller, tenant, command)
	if err != nil {
		writeManagementOperationError(c, err)
		return
	}
	writeManagedOperation(c, http.StatusAccepted, operation.View())
}

func (h *managementOperationHandler) requestTenantInvitation(c *gin.Context) {
	tenant, ok := managementTenant(c)
	if !ok {
		return
	}
	var command contract.RequestTenantInvitation
	if !decodeManagementBody(c, &command) || !managementKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	operation, err := h.service.AcceptForTenant(c.Request.Context(), h.caller, tenant, command)
	if err != nil {
		writeManagementOperationError(c, err)
		return
	}
	writeManagedOperation(c, http.StatusAccepted, operation.View())
}

func (h *managementOperationHandler) revokeTenantMember(c *gin.Context) {
	tenant, ok := managementTenant(c)
	if !ok {
		return
	}
	var command contract.RevokeTenantMember
	if !decodeManagementBody(c, &command) || !managementKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	if string(command.Subject) != c.Param("subject") {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "route member does not match revoke command subject")
		return
	}
	operation, err := h.service.AcceptForTenant(c.Request.Context(), h.caller, tenant, command)
	if err != nil {
		writeManagementOperationError(c, err)
		return
	}
	writeManagedOperation(c, http.StatusAccepted, operation.View())
}

func (h *managementOperationHandler) applyModuleChange(c *gin.Context) {
	tenant, ok := managementTenant(c)
	if !ok {
		return
	}
	var command contract.ApplyModuleChange
	if !decodeManagementBody(c, &command) || !managementKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	operation, err := h.service.AcceptForTenant(c.Request.Context(), h.caller, tenant, command)
	if err != nil {
		writeManagementOperationError(c, err)
		return
	}
	writeManagedOperation(c, http.StatusAccepted, operation.View())
}

func (h *managementOperationHandler) planModuleChange(c *gin.Context) {
	tenant, ok := managementTenant(c)
	if !ok {
		return
	}
	var request contract.PlanModuleChange
	if !decodeManagementBody(c, &request) {
		return
	}
	if err := contract.ValidatePlanModuleChange(request); err != nil {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, err.Error())
		return
	}
	plan, err := h.moduleLifecyclePlanner.Plan(c.Request.Context(), tenant, request)
	if err != nil {
		writeManagementOperationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": plan})
}

func (h *managementOperationHandler) get(c *gin.Context) {
	id, err := contract.ParseOperationID(c.Param("id"))
	if err != nil {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "invalid management operation ID")
		return
	}
	operation, err := h.service.Get(c.Request.Context(), h.caller, id)
	if err != nil {
		writeManagementOperationError(c, err)
		return
	}
	writeManagedOperation(c, http.StatusOK, operation.View())
}

func (h *managementOperationHandler) reconcileTenantState(c *gin.Context) {
	id, err := contract.ParseOperationID(c.Param("id"))
	if err != nil {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "invalid management operation ID")
		return
	}
	var command contract.ReconcileTenantStateOperation
	if !decodeManagementBody(c, &command) || !managementKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	if err := contract.ValidateReconcileTenantStateOperation(command); err != nil {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "tenant-state reconciliation command is invalid")
		return
	}
	operation, err := h.service.ReconcileTenantState(c.Request.Context(), h.caller, id, command)
	if err != nil {
		writeManagementOperationError(c, err)
		return
	}
	writeManagedOperation(c, http.StatusAccepted, operation.View())
}

func (h *managementOperationHandler) reconcileTenantProvision(c *gin.Context) {
	if !h.provisionRecoveryEnabled {
		writeManagementError(c, http.StatusNotFound, contract.CodeNotFound, "provisioning recovery is not enabled")
		return
	}
	id, err := contract.ParseOperationID(c.Param("id"))
	if err != nil {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "invalid management operation ID")
		return
	}
	var command contract.ReconcileTenantProvisionOperation
	if !decodeManagementBody(c, &command) || !managementKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	if err := contract.ValidateReconcileTenantProvisionOperation(command); err != nil {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "tenant provisioning reconciliation command is invalid")
		return
	}
	operation, err := h.service.ReconcileTenantProvision(c.Request.Context(), h.caller, id, command)
	if err != nil {
		writeManagementOperationError(c, err)
		return
	}
	writeManagedOperation(c, http.StatusAccepted, operation.View())
}

func (h *managementOperationHandler) reconcileTenantPackage(c *gin.Context) {
	if !h.packageRecoveryEnabled {
		writeManagementError(c, http.StatusNotFound, contract.CodeNotFound, "package application recovery is not enabled")
		return
	}
	id, err := contract.ParseOperationID(c.Param("id"))
	if err != nil {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "invalid management operation ID")
		return
	}
	var command contract.ReconcileTenantPackageOperation
	if !decodeManagementBody(c, &command) || !managementKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	if err := contract.ValidateReconcileTenantPackageOperation(command); err != nil {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "package application reconciliation command is invalid")
		return
	}
	operation, err := h.service.ReconcileTenantPackage(c.Request.Context(), h.caller, id, command)
	if err != nil {
		writeManagementOperationError(c, err)
		return
	}
	writeManagedOperation(c, http.StatusAccepted, operation.View())
}

func (h *managementOperationHandler) reconcileTenantDelete(c *gin.Context) {
	if !h.deleteRecoveryEnabled {
		writeManagementError(c, http.StatusNotFound, contract.CodeNotFound, "tenant deletion recovery is not enabled")
		return
	}
	id, err := contract.ParseOperationID(c.Param("id"))
	if err != nil {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "invalid management operation ID")
		return
	}
	var command contract.ReconcileTenantDeleteOperation
	if !decodeManagementBody(c, &command) || !managementKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	if err := contract.ValidateReconcileTenantDeleteOperation(command); err != nil {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "tenant deletion reconciliation command is invalid")
		return
	}
	operation, err := h.service.ReconcileTenantDelete(c.Request.Context(), h.caller, id, command)
	if err != nil {
		writeManagementOperationError(c, err)
		return
	}
	writeManagedOperation(c, http.StatusAccepted, operation.View())
}

func (h *managementOperationHandler) reconcileTenantInvitation(c *gin.Context) {
	id, err := contract.ParseOperationID(c.Param("id"))
	if err != nil {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "invalid management operation ID")
		return
	}
	var command contract.ReconcileTenantInvitationOperation
	if !decodeManagementBody(c, &command) || !managementKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	operation, err := h.service.Get(c.Request.Context(), h.caller, id)
	if err != nil {
		writeManagementOperationError(c, err)
		return
	}
	if operation.Kind != management.CommandTenantInvitation {
		writeManagementError(c, http.StatusConflict, contract.CodeConflict, "operation is not a tenant invitation")
		return
	}
	if operation.State == contract.OperationRecoveryRequired {
		if err := h.invitationReconciler.ReconcileInvitation(c.Request.Context(), operation, command); err != nil {
			writeManagementOperationError(c, err)
			return
		}
	}
	updated, err := h.service.ReconcileTenantInvitation(c.Request.Context(), h.caller, id, command)
	if err != nil {
		writeManagementOperationError(c, err)
		return
	}
	writeManagedOperation(c, http.StatusAccepted, updated.View())
}

func writeManagedOperation(c *gin.Context, status int, operation contract.ManagedOperation) {
	if err := contract.ValidateManagedOperation(operation); err != nil {
		writeManagementError(c, http.StatusServiceUnavailable, contract.CodeDependencyUnavailable, "management operation store returned an invalid result")
		return
	}
	c.JSON(status, Response{Data: operation})
}

func managementTenant(c *gin.Context) (contract.TenantID, bool) {
	tenant, err := contract.ParseTenantID(c.Param("id"))
	if err != nil {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "invalid tenant ID")
		return "", false
	}
	return tenant, true
}

func decodeManagementBody(c *gin.Context, target any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, managementBodyLimit)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		message := "management command body is invalid"
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			message = "management command body exceeds 1 MiB"
		}
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, message)
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "management command body must contain exactly one JSON value")
		return false
	}
	return true
}

func managementKeyMatches(c *gin.Context, body contract.IdempotencyKey) bool {
	header := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if header != "" && header != string(body) {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "Idempotency-Key header does not match command metadata")
		return false
	}
	return true
}

func writeManagementOperationError(c *gin.Context, err error) {
	var apiError *contract.Error
	if !errors.As(err, &apiError) {
		writeManagementError(c, http.StatusInternalServerError, contract.CodeInternal, "management operation could not be processed")
		return
	}
	status := http.StatusInternalServerError
	switch apiError.Type {
	case contract.CodeUnauthenticated:
		status = http.StatusUnauthorized
	case contract.CodePermissionDenied:
		status = http.StatusForbidden
	case contract.CodeNotFound:
		status = http.StatusNotFound
	case contract.CodeValidationFailed, contract.CodeUnsupportedVersion:
		status = http.StatusBadRequest
	case contract.CodeConflict, contract.CodeIdempotencyKeyReused, contract.CodeStaleSubject, contract.CodeLeaseLost:
		status = http.StatusConflict
	case contract.CodeResourceExhausted:
		status = http.StatusServiceUnavailable
	case contract.CodeDependencyUnavailable, contract.CodeOutcomeUnknown:
		status = http.StatusServiceUnavailable
	case contract.CodeDeadlineExceeded:
		status = http.StatusGatewayTimeout
	}
	message := apiError.Message
	if status >= 500 {
		message = "management operation could not be processed"
	}
	writeManagementError(c, status, apiError.Type, message)
}
