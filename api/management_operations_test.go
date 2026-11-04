package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/management"
	"github.com/asenawritescode/kora/site"
	"github.com/gin-gonic/gin"
)

type managementOperationServiceStub struct {
	accepted              management.Operation
	acceptErr             error
	got                   management.Operation
	getErr                error
	recovery              management.Operation
	recoveryErr           error
	provisionRecovery     management.Operation
	provisionRecoveryErr  error
	packageRecovery       management.Operation
	packageRecoveryErr    error
	deleteRecovery        management.Operation
	deleteRecoveryErr     error
	invitationRecovery    management.Operation
	invitationRecoveryErr error
	caller                contract.PrincipalID
	tenant                contract.TenantID
	command               any
}

func (s *managementOperationServiceStub) Accept(_ context.Context, caller contract.PrincipalID, command any) (management.Operation, error) {
	s.caller, s.command = caller, command
	return s.accepted, s.acceptErr
}
func (s *managementOperationServiceStub) AcceptForTenant(_ context.Context, caller contract.PrincipalID, tenant contract.TenantID, command any) (management.Operation, error) {
	s.caller, s.tenant, s.command = caller, tenant, command
	return s.accepted, s.acceptErr
}
func (s *managementOperationServiceStub) Get(_ context.Context, caller contract.PrincipalID, _ contract.OperationID) (management.Operation, error) {
	s.caller = caller
	return s.got, s.getErr
}
func (s *managementOperationServiceStub) ReconcileTenantState(_ context.Context, caller contract.PrincipalID, _ contract.OperationID, command contract.ReconcileTenantStateOperation) (management.Operation, error) {
	s.caller, s.command = caller, command
	return s.recovery, s.recoveryErr
}
func (s *managementOperationServiceStub) ReconcileTenantProvision(_ context.Context, caller contract.PrincipalID, _ contract.OperationID, command contract.ReconcileTenantProvisionOperation) (management.Operation, error) {
	s.caller, s.command = caller, command
	return s.provisionRecovery, s.provisionRecoveryErr
}
func (s *managementOperationServiceStub) ReconcileTenantPackage(_ context.Context, caller contract.PrincipalID, _ contract.OperationID, command contract.ReconcileTenantPackageOperation) (management.Operation, error) {
	s.caller, s.command = caller, command
	return s.packageRecovery, s.packageRecoveryErr
}
func (s *managementOperationServiceStub) ReconcileTenantDelete(_ context.Context, caller contract.PrincipalID, _ contract.OperationID, command contract.ReconcileTenantDeleteOperation) (management.Operation, error) {
	s.caller, s.command = caller, command
	return s.deleteRecovery, s.deleteRecoveryErr
}
func (s *managementOperationServiceStub) ReconcileTenantInvitation(_ context.Context, caller contract.PrincipalID, _ contract.OperationID, command contract.ReconcileTenantInvitationOperation) (management.Operation, error) {
	s.caller, s.command = caller, command
	return s.invitationRecovery, s.invitationRecoveryErr
}

type invitationReconcilerStub struct {
	called    int
	operation management.Operation
	command   contract.ReconcileTenantInvitationOperation
	err       error
}

func (s *invitationReconcilerStub) ReconcileInvitation(_ context.Context, operation management.Operation, command contract.ReconcileTenantInvitationOperation) error {
	s.called++
	s.operation, s.command = operation, command
	return s.err
}

func managementOperationRouter(t *testing.T, service ManagementOperationService, capabilities map[ManagementCapability]bool) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	handler, err := NewManagementInspectHandler(managementDirectoryStub{}, "separate-management-secret", "deployment-a")
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	if err := handler.RegisterOperations(router, ManagementOperationRoutes{
		Service: service, Caller: "cloud-control-plane", Capabilities: capabilities, WorkerReady: true,
		InvitationReconciler:   &invitationReconcilerStub{},
		ModuleLifecyclePlanner: moduleLifecyclePlannerStub{},
		ModuleInspector:        moduleLifecyclePlannerStub{},
	}); err != nil {
		t.Fatal(err)
	}
	return router
}

type moduleLifecyclePlannerStub struct{}

func (moduleLifecyclePlannerStub) Plan(_ context.Context, _ contract.TenantID, request contract.PlanModuleChange) (contract.ModuleChangePlan, error) {
	return contract.ModuleChangePlan{ModuleID: request.ModuleID, TargetState: request.TargetState, BaseRevision: request.ExpectedRevision, PlanHash: strings.Repeat("a", 64)}, nil
}

func (moduleLifecyclePlannerStub) InspectModules(context.Context, contract.TenantID) (contract.ModuleInspection, error) {
	return contract.ModuleInspection{Installations: []contract.ModuleInstallation{}, ResourceOwners: []contract.ResourceOwnership{}, Bindings: []contract.BindingInspection{}, CapabilityAvailability: []contract.CapabilityInspection{}, BlockingReasons: []contract.ModuleBlocker{}}, nil
}

func (moduleLifecyclePlannerStub) InspectExecution(_ context.Context, _ contract.TenantID, executionID string) (contract.ExecutionInspection, error) {
	return contract.ExecutionInspection{ExecutionID: executionID, CallerModuleID: "sales.orders", OwnerModuleID: "finance.ar", ProviderID: "ar-provider", BindingRevision: 3, DeliveryState: "succeeded", ReconciliationState: "not_required"}, nil
}

func allManagementCapabilities() map[ManagementCapability]bool {
	return map[ManagementCapability]bool{
		ManagementProvisionCapability:  true,
		ManagementPackageCapability:    true,
		ManagementStateCapability:      true,
		ManagementStateRecovery:        true,
		ManagementProvisionRecovery:    true,
		ManagementPackageRecovery:      true,
		ManagementDeleteCapability:     true,
		ManagementDeleteRecovery:       true,
		ManagementInvitationCapability: true,
		ManagementInvitationRecovery:   true,
		ManagementModuleLifecycle:      true,
		ManagementOperationRead:        true,
	}
}

func TestManagementInvitationUsesRouteTenantAndVerifiedCaller(t *testing.T) {
	operation := management.Operation{ID: "operation-invite-1", Tenant: "tenant-a", Caller: "cloud-control-plane", State: contract.OperationRequested, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	service := &managementOperationServiceStub{accepted: operation}
	router := managementOperationRouter(t, service, allManagementCapabilities())
	body := `{"meta":{"idempotency_key":"invite-owner-01"},"email":"owner@example.test"}`
	response := managementPost(router, "/api/management/v2/tenants/tenant-a/invitation", body, "invite-owner-01")
	if response.Code != http.StatusAccepted {
		t.Fatalf("invitation status=%d body=%s", response.Code, response.Body.String())
	}
	if service.caller != "cloud-control-plane" || service.tenant != "tenant-a" {
		t.Fatalf("invitation identity caller=%q tenant=%q", service.caller, service.tenant)
	}
	command, ok := service.command.(contract.RequestTenantInvitation)
	if !ok || command.Email != "owner@example.test" || command.Meta.IdempotencyKey != "invite-owner-01" {
		t.Fatalf("invitation command=%#v", service.command)
	}
	without := allManagementCapabilities()
	delete(without, ManagementInvitationCapability)
	delete(without, ManagementInvitationRecovery)
	service.command = nil
	response = managementPost(managementOperationRouter(t, service, without), "/api/management/v2/tenants/tenant-a/invitation", body, "invite-owner-01")
	if response.Code != http.StatusNotFound || service.command != nil {
		t.Fatalf("disabled invitation status=%d command=%#v", response.Code, service.command)
	}
}

func TestManagementModuleLifecyclePlanAndApplyAreTenantScoped(t *testing.T) {
	operation := management.Operation{ID: "operation-module-1", Tenant: "tenant-a", Caller: "cloud-control-plane", State: contract.OperationRequested, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	service := &managementOperationServiceStub{accepted: operation}
	router := managementOperationRouter(t, service, allManagementCapabilities())
	planResponse := managementPost(router, "/api/management/v2/tenants/tenant-a/modules/plan", `{"module_id":"billing.core","target_state":"draining","expected_revision":3}`, "")
	if planResponse.Code != http.StatusOK {
		t.Fatalf("plan status=%d body=%s", planResponse.Code, planResponse.Body.String())
	}
	body := `{"meta":{"idempotency_key":"module-change-01"},"module_id":"billing.core","target_state":"draining","plan_hash":"` + strings.Repeat("a", 64) + `","expected_revision":3}`
	response := managementPost(router, "/api/management/v2/tenants/tenant-a/modules/apply", body, "module-change-01")
	if response.Code != http.StatusAccepted {
		t.Fatalf("apply status=%d body=%s", response.Code, response.Body.String())
	}
	command, ok := service.command.(contract.ApplyModuleChange)
	if !ok || service.tenant != "tenant-a" || command.ModuleID != "billing.core" {
		t.Fatalf("accepted lifecycle command/tenant = %#v / %q", service.command, service.tenant)
	}
}

func TestManagementInvitationReconciliationVerifiesOutcomeBeforeRequeue(t *testing.T) {
	operation := management.Operation{ID: "operation-invite-1", Tenant: "tenant-a", Caller: "cloud-control-plane", Kind: management.CommandTenantInvitation, State: contract.OperationRecoveryRequired, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	service := &managementOperationServiceStub{got: operation, invitationRecovery: management.Operation{ID: operation.ID, Tenant: operation.Tenant, State: contract.OperationRequested}}
	reconciler := &invitationReconcilerStub{}
	gin.SetMode(gin.TestMode)
	handler, err := NewManagementInspectHandler(managementDirectoryStub{}, "separate-management-secret", "deployment-a")
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	caps := allManagementCapabilities()
	if err := handler.RegisterOperations(router, ManagementOperationRoutes{Service: service, Caller: "cloud-control-plane", Capabilities: caps, WorkerReady: true, InvitationReconciler: reconciler, ModuleLifecyclePlanner: moduleLifecyclePlannerStub{}, ModuleInspector: moduleLifecyclePlannerStub{}}); err != nil {
		t.Fatal(err)
	}
	body := `{"meta":{"idempotency_key":"reconcile-invite-01"},"outcome":"not_delivered","reason":"provider logs confirm the message was not accepted"}`
	response := managementPost(router, "/api/management/v2/operations/operation-invite-1/reconcile-tenant-invitation", body, "reconcile-invite-01")
	if response.Code != http.StatusAccepted {
		t.Fatalf("reconciliation status=%d body=%s", response.Code, response.Body.String())
	}
	if reconciler.called != 1 || reconciler.operation.ID != operation.ID || reconciler.command.Outcome != contract.InvitationNotDelivered {
		t.Fatalf("tenant receipt reconciliation=%+v", reconciler)
	}
	if service.caller != "cloud-control-plane" {
		t.Fatalf("reconciliation caller=%q", service.caller)
	}
	if command, ok := service.command.(contract.ReconcileTenantInvitationOperation); !ok || command.Outcome != contract.InvitationNotDelivered {
		t.Fatalf("reconciliation command=%#v", service.command)
	}

	// A replay after the operation has left recovery-required is answered from
	// the management operation journal and does not touch the tenant receipt.
	service.got.State = contract.OperationRequested
	response = managementPost(router, "/api/management/v2/operations/operation-invite-1/reconcile-tenant-invitation", body, "reconcile-invite-01")
	if response.Code != http.StatusAccepted || reconciler.called != 1 {
		t.Fatalf("replay status=%d calls=%d body=%s", response.Code, reconciler.called, response.Body.String())
	}

	withoutRecovery := allManagementCapabilities()
	delete(withoutRecovery, ManagementInvitationRecovery)
	service.command = nil
	response = managementPost(managementOperationRouter(t, service, withoutRecovery), "/api/management/v2/operations/operation-invite-1/reconcile-tenant-invitation", body, "reconcile-invite-01")
	if response.Code != http.StatusNotFound || service.command != nil {
		t.Fatalf("disabled invitation recovery status=%d command=%#v", response.Code, service.command)
	}
}

func TestManagementCapabilitiesOnlyAdvertiseEnabledOperations(t *testing.T) {
	enabled := map[ManagementCapability]bool{
		ManagementProvisionCapability: true,
		ManagementProvisionRecovery:   true,
		ManagementOperationRead:       true,
		ManagementPackageCapability:   false,
	}
	router := managementOperationRouter(t, &managementOperationServiceStub{}, enabled)
	request := httptest.NewRequest(http.MethodGet, "/api/management/v2/capabilities", nil)
	request.Header.Set("Authorization", "Bearer separate-management-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("capabilities status = %d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data contract.ManagementCapabilitiesV2 `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal("decode Engine management capabilities:", err)
	}
	if err := contract.ValidateManagementCapabilitiesV2(envelope.Data); err != nil {
		t.Fatalf("invalid Engine capability response: %v", err)
	}
	want := []contract.ManagementCapability{ManagementOperationRead, ManagementProvisionCapability, ManagementProvisionRecovery}
	if envelope.Data.Version != 2 || len(envelope.Data.Capabilities) != len(want) {
		t.Fatalf("capabilities = %+v, want %v", envelope.Data, want)
	}
	for i := range want {
		if envelope.Data.Capabilities[i] != want[i] {
			t.Fatalf("capabilities = %v, want %v", envelope.Data.Capabilities, want)
		}
	}
	request = httptest.NewRequest(http.MethodGet, "/api/management/v2/capabilities", nil)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated capabilities status = %d, want 401", response.Code)
	}
}

func TestManagementPackageReconciliationRequiresCapabilityAndUsesVerifiedCaller(t *testing.T) {
	operation := management.Operation{ID: "operation-1", Tenant: "tenant-a", Caller: "cloud-control-plane", State: contract.OperationRequested, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	service := &managementOperationServiceStub{packageRecovery: operation}
	router := managementOperationRouter(t, service, allManagementCapabilities())
	body := `{"meta":{"idempotency_key":"reconcile-package-01"},"reason":"operator inspected the activation journal and confirmed retry"}`
	response := managementPost(router, "/api/management/v2/operations/operation-1/reconcile-tenant-package", body, "reconcile-package-01")
	if response.Code != http.StatusAccepted || !contains(response.Body.String(), `"state":"requested"`) {
		t.Fatalf("package reconciliation status=%d body=%s", response.Code, response.Body.String())
	}
	if service.caller != "cloud-control-plane" {
		t.Fatalf("package reconciliation caller = %q", service.caller)
	}
	command, ok := service.command.(contract.ReconcileTenantPackageOperation)
	if !ok || command.Reason == "" || command.Meta.IdempotencyKey != "reconcile-package-01" {
		t.Fatalf("package reconciliation command = %#v", service.command)
	}

	withoutRecovery := allManagementCapabilities()
	delete(withoutRecovery, ManagementPackageRecovery)
	disabledRouter := managementOperationRouter(t, service, withoutRecovery)
	service.command = nil
	response = managementPost(disabledRouter, "/api/management/v2/operations/operation-1/reconcile-tenant-package", body, "reconcile-package-01")
	if response.Code != http.StatusNotFound || service.command != nil {
		t.Fatalf("disabled package reconciliation status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestManagementDeleteReconciliationUsesVerifiedCallerAndRequiresCapability(t *testing.T) {
	operation := management.Operation{ID: "operation-1", Tenant: "tenant-a", Caller: "cloud-control-plane", State: contract.OperationRequested, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	service := &managementOperationServiceStub{deleteRecovery: operation}
	router := managementOperationRouter(t, service, allManagementCapabilities())
	body := `{"meta":{"idempotency_key":"reconcile-delete-01"},"reason":"checkpoint provider recovered and allocation identity was rechecked"}`
	response := managementPost(router, "/api/management/v2/operations/operation-1/reconcile-tenant-delete", body, "reconcile-delete-01")
	if response.Code != http.StatusAccepted || !contains(response.Body.String(), `"state":"requested"`) {
		t.Fatalf("delete reconciliation status=%d body=%s", response.Code, response.Body.String())
	}
	if service.caller != "cloud-control-plane" {
		t.Fatalf("delete reconciliation caller=%q", service.caller)
	}
	command, ok := service.command.(contract.ReconcileTenantDeleteOperation)
	if !ok || command.Meta.IdempotencyKey != "reconcile-delete-01" {
		t.Fatalf("delete reconciliation command=%#v", service.command)
	}
	service.command = nil
	response = managementPost(router, "/api/management/v2/operations/operation-1/reconcile-tenant-delete", `{"meta":{"idempotency_key":"reconcile-delete-02"},"reason":" "}`, "reconcile-delete-02")
	if response.Code != http.StatusBadRequest || service.command != nil {
		t.Fatalf("empty delete reason status=%d command=%#v", response.Code, service.command)
	}
	withoutRecovery := allManagementCapabilities()
	delete(withoutRecovery, ManagementDeleteRecovery)
	disabled := managementOperationRouter(t, service, withoutRecovery)
	response = managementPost(disabled, "/api/management/v2/operations/operation-1/reconcile-tenant-delete", body, "reconcile-delete-01")
	if response.Code != http.StatusNotFound || service.command != nil {
		t.Fatalf("disabled deletion reconciliation status=%d command=%#v", response.Code, service.command)
	}
}

func TestManagementTenantStateReconciliationRequiresBoundedReasonAndUsesVerifiedCaller(t *testing.T) {
	operation := management.Operation{
		ID: "operation-1", Tenant: "tenant-a", Caller: "cloud-control-plane",
		State: contract.OperationRequested, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	service := &managementOperationServiceStub{recovery: operation}
	router := managementOperationRouter(t, service, allManagementCapabilities())
	body := `{"meta":{"idempotency_key":"reconcile-state-01"},"reason":"operator confirmed current tenant state and requested safe convergence retry"}`
	response := managementPost(router, "/api/management/v2/operations/operation-1/reconcile-tenant-state", body, "reconcile-state-01")
	if response.Code != http.StatusAccepted || !contains(response.Body.String(), `"state":"requested"`) {
		t.Fatalf("reconciliation status=%d body=%s", response.Code, response.Body.String())
	}
	if service.caller != "cloud-control-plane" {
		t.Fatalf("reconciliation caller = %q", service.caller)
	}
	command, ok := service.command.(contract.ReconcileTenantStateOperation)
	if !ok || command.Reason == "" || command.Meta.IdempotencyKey != "reconcile-state-01" {
		t.Fatalf("reconciliation command = %#v", service.command)
	}

	service.command = nil
	response = managementPost(router, "/api/management/v2/operations/operation-1/reconcile-tenant-state", `{"meta":{"idempotency_key":"reconcile-state-02"},"reason":""}`, "reconcile-state-02")
	if response.Code != http.StatusBadRequest || service.command != nil {
		t.Fatalf("empty reconciliation reason status=%d body=%s command=%#v", response.Code, response.Body.String(), service.command)
	}
}

func TestManagementProvisionReconciliationUsesVerifiedCallerAndRequiresCapability(t *testing.T) {
	operation := management.Operation{ID: "operation-1", Tenant: "tenant-a", Caller: "cloud-control-plane", State: contract.OperationRequested, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	service := &managementOperationServiceStub{provisionRecovery: operation}
	router := managementOperationRouter(t, service, allManagementCapabilities())
	body := `{"meta":{"idempotency_key":"reconcile-provision-01"},"reason":"operator confirmed this is a base provisioning retry without package effects"}`
	response := managementPost(router, "/api/management/v2/operations/operation-1/reconcile-tenant-provision", body, "reconcile-provision-01")
	if response.Code != http.StatusAccepted || !contains(response.Body.String(), `"state":"requested"`) {
		t.Fatalf("provision reconciliation status=%d body=%s", response.Code, response.Body.String())
	}
	if service.caller != "cloud-control-plane" {
		t.Fatalf("provision reconciliation caller = %q", service.caller)
	}
	command, ok := service.command.(contract.ReconcileTenantProvisionOperation)
	if !ok || command.Reason == "" || command.Meta.IdempotencyKey != "reconcile-provision-01" {
		t.Fatalf("provision reconciliation command = %#v", service.command)
	}

	service.command = nil
	response = managementPost(router, "/api/management/v2/operations/operation-1/reconcile-tenant-provision", `{"meta":{"idempotency_key":"reconcile-provision-02"},"reason":""}`, "reconcile-provision-02")
	if response.Code != http.StatusBadRequest || service.command != nil {
		t.Fatalf("empty provision reconciliation status=%d body=%s command=%#v", response.Code, response.Body.String(), service.command)
	}

	withoutRecovery := allManagementCapabilities()
	delete(withoutRecovery, ManagementProvisionRecovery)
	disabledRouter := managementOperationRouter(t, service, withoutRecovery)
	response = managementPost(disabledRouter, "/api/management/v2/operations/operation-1/reconcile-tenant-provision", body, "reconcile-provision-01")
	if response.Code != http.StatusNotFound || service.command != nil {
		t.Fatalf("disabled provision reconciliation status=%d body=%s", response.Code, response.Body.String())
	}
}

func managementPost(router http.Handler, path, body, idempotency string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer separate-management-secret")
	request.Header.Set("Content-Type", "application/json")
	if idempotency != "" {
		request.Header.Set("Idempotency-Key", idempotency)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestManagementOperationRoutesAcceptDurableCommandsAndScopeTenant(t *testing.T) {
	operation := management.Operation{
		ID: "operation-1", Tenant: "tenant-a", Caller: "cloud-control-plane",
		State: contract.OperationRequested, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	service := &managementOperationServiceStub{accepted: operation, got: operation}
	router := managementOperationRouter(t, service, allManagementCapabilities())
	body := `{"meta":{"idempotency_key":"cloud-operation-01"},"tenant_id":"tenant-a","bootstrap_admin_email":"admin@example.test","primary_alias":"tenant.example.test","aliases":[],"deployment_id":"deployment-a","database_allocation_ref":"mysql:allocation-a"}`
	response := managementPost(router, "/api/management/v2/tenants", body, "cloud-operation-01")
	if response.Code != http.StatusAccepted || !contains(response.Body.String(), `"data":{"id":"operation-1"`) {
		t.Fatalf("provision status=%d body=%s", response.Code, response.Body.String())
	}
	if service.caller != "cloud-control-plane" {
		t.Fatalf("verified caller = %q", service.caller)
	}
	if _, ok := service.command.(contract.ProvisionTenant); !ok {
		t.Fatalf("accepted command type = %T", service.command)
	}

	applyBody := `{"meta":{"idempotency_key":"cloud-operation-02"},"tenant_id":"tenant-b","artifact_ref":"artifact:pack-a","digest":"` + strings.Repeat("a", 64) + `","expected_config_revision":"1"}`
	response = managementPost(router, "/api/management/v2/tenants/tenant-a/packages", applyBody, "cloud-operation-02")
	if response.Code != http.StatusBadRequest || !contains(response.Body.String(), `"type":"VALIDATION_FAILED"`) {
		t.Fatalf("mismatched route tenant status=%d body=%s", response.Code, response.Body.String())
	}
	if service.tenant != "" {
		t.Fatalf("mismatched route reached service with tenant %q", service.tenant)
	}
}

func TestManagementModuleInspectionRoutesReturnRedactedTypedProjections(t *testing.T) {
	router := managementOperationRouter(t, &managementOperationServiceStub{}, allManagementCapabilities())
	request := httptest.NewRequest(http.MethodGet, "/api/management/v2/tenants/tenant-a/modules/inspection", nil)
	request.Header.Set("Authorization", "Bearer separate-management-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !contains(response.Body.String(), `"config_revision":0`) || !contains(response.Body.String(), `"installations":[]`) {
		t.Fatalf("tenant module inspection response=%d %s", response.Code, response.Body.String())
	}
	for _, forbidden := range []string{"credential_ref", "secret", "password", "email", "payload"} {
		if contains(response.Body.String(), forbidden) {
			t.Fatalf("module inspection leaked %q: %s", forbidden, response.Body.String())
		}
	}

	executionID := contract.NewID()
	request = httptest.NewRequest(http.MethodGet, "/api/management/v2/tenants/tenant-a/executions/"+executionID, nil)
	request.Header.Set("Authorization", "Bearer separate-management-secret")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !contains(response.Body.String(), `"caller_module_id":"sales.orders"`) || contains(response.Body.String(), `"payload"`) {
		t.Fatalf("tenant execution inspection response=%d %s", response.Code, response.Body.String())
	}
}

func TestManagementOperationRoutesRejectUnknownFieldsAndConflictingKeys(t *testing.T) {
	service := &managementOperationServiceStub{accepted: management.Operation{ID: "operation-1", Tenant: "tenant-a", State: contract.OperationRequested}}
	router := managementOperationRouter(t, service, allManagementCapabilities())
	valid := `{"meta":{"idempotency_key":"cloud-operation-01"},"tenant_id":"tenant-a","bootstrap_admin_email":"admin@example.test","primary_alias":"tenant.example.test","aliases":[],"deployment_id":"deployment-a","database_allocation_ref":"mysql:allocation-a"}`
	unknown := strings.TrimSuffix(valid, "}") + `,"database_dsn":"mysql://user:secret@host/db"}`
	response := managementPost(router, "/api/management/v2/tenants", unknown, "cloud-operation-01")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d body=%s", response.Code, response.Body.String())
	}
	response = managementPost(router, "/api/management/v2/tenants", valid, "different-key")
	if response.Code != http.StatusBadRequest || !contains(response.Body.String(), "does not match") {
		t.Fatalf("conflicting idempotency key status=%d body=%s", response.Code, response.Body.String())
	}
	if service.command != nil {
		t.Fatal("invalid command reached operation service")
	}
}

func TestManagementProvisionRejectsPackageDigestUntilProvisionExecutorSupportsIt(t *testing.T) {
	for _, packageApplyEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "package_apply_disabled", true: "standalone_package_apply_enabled"}[packageApplyEnabled], func(t *testing.T) {
			service := &managementOperationServiceStub{}
			capabilities := map[ManagementCapability]bool{
				ManagementProvisionCapability: true,
				ManagementOperationRead:       true,
			}
			if packageApplyEnabled {
				capabilities[ManagementPackageCapability] = true
			}
			router := managementOperationRouter(t, service, capabilities)
			body := `{"meta":{"idempotency_key":"provision-package-01"},"tenant_id":"tenant-a","bootstrap_admin_email":"admin@example.test","primary_alias":"tenant.example.test","aliases":[],"deployment_id":"deployment-a","database_allocation_ref":"mysql:allocation-a","package_digest":"` + strings.Repeat("a", 64) + `"}`
			response := managementPost(router, "/api/management/v2/tenants", body, "provision-package-01")
			if response.Code != http.StatusNotImplemented || !contains(response.Body.String(), "package-bearing provisioning is not enabled") {
				t.Fatalf("provision with package status=%d body=%s", response.Code, response.Body.String())
			}
			if service.command != nil {
				t.Fatalf("unsupported package command was durably accepted: %#v", service.command)
			}
		})
	}
}

func TestManagementOperationRoutesRegisterOnlyReadyCapabilitiesAndBoundBodies(t *testing.T) {
	service := &managementOperationServiceStub{}
	capabilities := map[ManagementCapability]bool{
		ManagementStateCapability: true,
		ManagementOperationRead:   true,
	}
	handler, err := NewManagementInspectHandler(managementDirectoryStub{}, "secret", "deployment-a")
	if err != nil {
		t.Fatal(err)
	}
	stateRouter := gin.New()
	if err := handler.RegisterOperations(stateRouter, ManagementOperationRoutes{Service: service, Caller: "cloud-control-plane", Capabilities: capabilities, WorkerReady: true}); err != nil {
		t.Fatalf("register state-only operation capability: %v", err)
	}
	unsupportedRequest := httptest.NewRequest(http.MethodPost, "/api/management/v2/tenants", strings.NewReader("{}"))
	unsupportedRequest.Header.Set("Authorization", "Bearer secret")
	unsupportedResponse := httptest.NewRecorder()
	stateRouter.ServeHTTP(unsupportedResponse, unsupportedRequest)
	unsupported := unsupportedResponse
	if unsupported.Code != http.StatusNotFound {
		t.Fatalf("unconfigured provisioning route status=%d body=%s", unsupported.Code, unsupported.Body.String())
	}
	unsupportedDelete := httptest.NewRecorder()
	deleteRequest := httptest.NewRequest(http.MethodPost, "/api/management/v2/tenants/tenant-a/delete", strings.NewReader(`{"meta":{"idempotency_key":"delete-operation-1"},"recovery_checkpoint_ref":"checkpoint:1"}`))
	deleteRequest.Header.Set("Authorization", "Bearer secret")
	deleteRequest.Header.Set("Content-Type", "application/json")
	stateRouter.ServeHTTP(unsupportedDelete, deleteRequest)
	if unsupportedDelete.Code != http.StatusNotFound {
		t.Fatalf("unconfigured deletion route status=%d body=%s", unsupportedDelete.Code, unsupportedDelete.Body.String())
	}
	missingRead := gin.New()
	if err := handler.RegisterOperations(missingRead, ManagementOperationRoutes{
		Service: service, Caller: "cloud-control-plane", WorkerReady: true,
		Capabilities: map[ManagementCapability]bool{ManagementStateCapability: true},
	}); err == nil {
		t.Fatal("state management registered without operation read capability")
	}
	if err := handler.RegisterOperations(gin.New(), ManagementOperationRoutes{Service: service, Caller: "cloud-control-plane", Capabilities: allManagementCapabilities()}); err == nil {
		t.Fatal("management mutation routes were registered without a ready worker")
	}
	router := managementOperationRouter(t, service, allManagementCapabilities())
	response := managementPost(router, "/api/management/v2/tenants", strings.Repeat(" ", managementBodyLimit+1), "")
	if response.Code != http.StatusBadRequest || !contains(response.Body.String(), "exceeds 1 MiB") {
		t.Fatalf("oversized body status=%d body=%s", response.Code, response.Body.String())
	}
	if service.command != nil {
		t.Fatal("oversized command reached operation service")
	}
}

func TestManagementOperationRouteErrorsUseSafeStructuredEnvelope(t *testing.T) {
	service := &managementOperationServiceStub{getErr: contract.NewError(contract.CodeNotFound, "management operation not found")}
	router := managementOperationRouter(t, service, allManagementCapabilities())
	request := httptest.NewRequest(http.MethodGet, "/api/management/v2/operations/operation-1", nil)
	request.Header.Set("Authorization", "Bearer separate-management-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || !contains(response.Body.String(), `"error":{"type":"NOT_FOUND"`) {
		t.Fatalf("operation lookup status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestManagementOperationViewsDoNotExposePersistedCommands(t *testing.T) {
	service := &managementOperationServiceStub{got: management.Operation{
		ID: "operation-1", Tenant: "tenant-a", Caller: "cloud-control-plane", State: contract.OperationSucceeded,
		Command: json.RawMessage(`{"bootstrap_admin_email":"private@example.test","database_allocation_ref":"mysql:private"}`),
	}}
	router := managementOperationRouter(t, service, allManagementCapabilities())
	request := httptest.NewRequest(http.MethodGet, "/api/management/v2/operations/operation-1", nil)
	request.Header.Set("Authorization", "Bearer separate-management-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("operation lookup status=%d body=%s", response.Code, response.Body.String())
	}
	for _, forbidden := range []string{"command", "private@example.test", "mysql:private", "cloud-control-plane"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("operation view leaked %q: %s", forbidden, response.Body.String())
		}
	}
}

var _ ManagementDirectory = (*site.SQLSiteRegistry)(nil)
