package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/management"
	"github.com/gin-gonic/gin"
)

type memberPlannerStub struct {
	tenant  contract.TenantID
	request contract.PlanTenantMember
	plan    contract.TenantMemberPlan
	err     error
}

func (s *memberPlannerStub) PlanMember(_ context.Context, tenant contract.TenantID, request contract.PlanTenantMember) (contract.TenantMemberPlan, error) {
	s.tenant, s.request = tenant, request
	return s.plan, s.err
}

func memberRouter(t *testing.T, service ManagementOperationService, planner ManagementTenantMemberPlanner, capabilities map[ManagementCapability]bool) (*gin.Engine, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	handler, err := NewManagementInspectHandler(managementDirectoryStub{}, "separate-management-secret", "deployment-a")
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	err = handler.RegisterOperations(router, ManagementOperationRoutes{
		Service: service, Caller: "cloud-control-plane", Capabilities: capabilities, WorkerReady: true, MemberPlanner: planner,
	})
	return router, err
}

func memberCapabilities(extra ...ManagementCapability) map[ManagementCapability]bool {
	capabilities := map[ManagementCapability]bool{ManagementOperationRead: true}
	for _, capability := range extra {
		capabilities[capability] = true
	}
	return capabilities
}

const (
	memberPlanBody  = `{"meta":{"idempotency_key":"member-plan-01"},"email":"cashier@example.test","roles":["Cashier"]}`
	memberGrantBody = `{"meta":{"idempotency_key":"inv_01K7INVITATION"},"invitation_id":"inv_01K7INVITATION","subject":"usr_member_1","email":"cashier@example.test","full_name":"Amina Otieno","roles":["Cashier"]}`
)

func TestManagementMemberPlanRouteIsSynchronousAndTenantScoped(t *testing.T) {
	problem := contract.TenantMemberProblem{Code: contract.TenantMemberRoleUnknown, Message: "role does not exist"}
	tests := []struct {
		name       string
		body       string
		planner    *memberPlannerStub
		wantStatus int
		wantAsked  bool
		wantAllow  bool
	}{
		{name: "allowed", body: memberPlanBody, planner: &memberPlannerStub{plan: contract.TenantMemberPlan{Allowed: true}}, wantStatus: http.StatusOK, wantAsked: true, wantAllow: true},
		{name: "refused plan is still 200", body: memberPlanBody, planner: &memberPlannerStub{plan: contract.TenantMemberPlan{Problems: []contract.TenantMemberProblem{problem}}}, wantStatus: http.StatusOK, wantAsked: true},
		{name: "invalid command", body: `{"meta":{"idempotency_key":"member-plan-01"},"email":"nope","roles":["Cashier"]}`, planner: &memberPlannerStub{}, wantStatus: http.StatusBadRequest},
		{name: "two roles", body: `{"meta":{"idempotency_key":"member-plan-01"},"email":"cashier@example.test","roles":["Cashier","Stock"]}`, planner: &memberPlannerStub{}, wantStatus: http.StatusBadRequest},
		{name: "unknown field", body: `{"meta":{"idempotency_key":"member-plan-01"},"email":"cashier@example.test","roles":["Cashier"],"admin":true}`, planner: &memberPlannerStub{}, wantStatus: http.StatusBadRequest},
		{name: "planner unavailable", body: memberPlanBody, planner: &memberPlannerStub{err: contract.NewError(contract.CodeDependencyUnavailable, "tenant is not ready")}, wantStatus: http.StatusServiceUnavailable, wantAsked: true},
		{name: "planner returns inconsistent plan", body: memberPlanBody, planner: &memberPlannerStub{plan: contract.TenantMemberPlan{}}, wantStatus: http.StatusInternalServerError, wantAsked: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &managementOperationServiceStub{}
			router, err := memberRouter(t, service, test.planner, memberCapabilities(ManagementMemberPlanCapability))
			if err != nil {
				t.Fatal(err)
			}
			response := managementPost(router, "/api/management/v2/tenants/tenant-a/members/plan", test.body, "")
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d want %d body=%s", response.Code, test.wantStatus, response.Body.String())
			}
			if asked := test.planner.tenant != ""; asked != test.wantAsked {
				t.Fatalf("planner asked=%v want %v", asked, test.wantAsked)
			}
			if service.command != nil {
				t.Fatal("a plan created a durable operation")
			}
			if test.wantStatus != http.StatusOK {
				return
			}
			var envelope struct {
				Data contract.TenantMemberPlan `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Data.Allowed != test.wantAllow {
				t.Fatalf("plan body=%s err=%v", response.Body.String(), err)
			}
			if test.planner.tenant != "tenant-a" || test.planner.request.Email != "cashier@example.test" || test.planner.request.Roles[0] != "Cashier" {
				t.Fatalf("planner saw tenant=%q request=%+v", test.planner.tenant, test.planner.request)
			}
		})
	}
}

func TestManagementMemberGrantRouteAcceptsDurableOperation(t *testing.T) {
	operation := management.Operation{ID: "operation-member-1", Tenant: "tenant-a", Caller: "cloud-control-plane", State: contract.OperationRequested, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	tests := []struct {
		name       string
		body       string
		header     string
		wantStatus int
		wantQueued bool
	}{
		{name: "accepted", body: memberGrantBody, header: "inv_01K7INVITATION", wantStatus: http.StatusAccepted, wantQueued: true},
		{name: "no header", body: memberGrantBody, wantStatus: http.StatusAccepted, wantQueued: true},
		{name: "header differs from command", body: memberGrantBody, header: "other-key-0001", wantStatus: http.StatusBadRequest},
		{name: "unknown field", body: `{"meta":{"idempotency_key":"inv_01K7INVITATION"},"invitation_id":"inv_01K7INVITATION","subject":"usr_member_1","email":"cashier@example.test","full_name":"A","roles":["Cashier"],"password":"x"}`, wantStatus: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &managementOperationServiceStub{accepted: operation}
			router, err := memberRouter(t, service, nil, memberCapabilities(ManagementMemberGrantCapability))
			if err != nil {
				t.Fatal(err)
			}
			response := managementPost(router, "/api/management/v2/tenants/tenant-a/members", test.body, test.header)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d want %d body=%s", response.Code, test.wantStatus, response.Body.String())
			}
			command, queued := service.command.(contract.GrantTenantMember)
			if queued != test.wantQueued {
				t.Fatalf("queued=%v want %v command=%#v", queued, test.wantQueued, service.command)
			}
			if queued && (service.tenant != "tenant-a" || service.caller != "cloud-control-plane" || command.Subject != "usr_member_1" || command.Roles[0] != "Cashier") {
				t.Fatalf("grant identity tenant=%q caller=%q command=%+v", service.tenant, service.caller, command)
			}
		})
	}
}

func TestManagementMemberRoutesRequireTheirCapabilities(t *testing.T) {
	operation := management.Operation{ID: "operation-member-1", Tenant: "tenant-a", State: contract.OperationRequested, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	service := &managementOperationServiceStub{accepted: operation}
	router, err := memberRouter(t, service, &memberPlannerStub{plan: contract.TenantMemberPlan{Allowed: true}}, memberCapabilities(ManagementStateCapability))
	if err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{"/api/management/v2/tenants/tenant-a/members/plan": memberPlanBody, "/api/management/v2/tenants/tenant-a/members": memberGrantBody} {
		if response := managementPost(router, path, body, ""); response.Code != http.StatusNotFound {
			t.Fatalf("%s without capability status=%d", path, response.Code)
		}
	}
	if service.command != nil {
		t.Fatal("a disabled member route reached the operation service")
	}
	if _, err := memberRouter(t, service, nil, memberCapabilities(ManagementMemberPlanCapability)); err == nil {
		t.Fatal("tenant.member.plan registered without a planner")
	}
	if _, err := memberRouter(t, service, nil, memberCapabilities(ManagementMemberGrantCapability)); err != nil {
		t.Fatalf("tenant.member.grant alone must register: %v", err)
	}
}

func TestManagementCapabilitiesAdvertiseTenantMemberOperations(t *testing.T) {
	router, err := memberRouter(t, &managementOperationServiceStub{}, &memberPlannerStub{}, memberCapabilities(ManagementMemberPlanCapability, ManagementMemberGrantCapability))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/management/v2/capabilities", nil)
	request.Header.Set("Authorization", "Bearer separate-management-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var envelope struct {
		Data contract.ManagementCapabilitiesV2 `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || contract.ValidateManagementCapabilitiesV2(envelope.Data) != nil {
		t.Fatalf("capabilities body=%s err=%v", response.Body.String(), err)
	}
	found := map[contract.ManagementCapability]bool{}
	for _, capability := range envelope.Data.Capabilities {
		found[capability] = true
	}
	if !found[contract.ManagementCapabilityTenantMemberPlan] || !found[contract.ManagementCapabilityTenantMemberGrant] || found[contract.ManagementCapabilityTenantMemberRevoke] {
		t.Fatalf("capabilities=%v", envelope.Data.Capabilities)
	}
}
