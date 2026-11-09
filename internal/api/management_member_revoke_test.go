package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/management"
)

func TestManagementMemberRevokeRouteIsTenantAndSubjectScoped(t *testing.T) {
	operation := management.Operation{ID: "operation-revoke-1", Tenant: "tenant-a", Caller: "cloud-control-plane", Kind: management.CommandTenantMemberRevoke, State: contract.OperationRequested, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	body := `{"meta":{"idempotency_key":"revoke-member-01"},"subject":"cloud-user-1","reason":"removed from team"}`
	for _, test := range []struct {
		name     string
		path     string
		header   string
		enabled  bool
		want     int
		accepted bool
	}{
		{"accepted", "/api/management/v2/tenants/tenant-a/members/cloud-user-1/revoke", "revoke-member-01", true, http.StatusAccepted, true},
		{"path subject differs from body subject", "/api/management/v2/tenants/tenant-a/members/cloud-user-2/revoke", "revoke-member-01", true, http.StatusBadRequest, false},
		{"header key differs from body key", "/api/management/v2/tenants/tenant-a/members/cloud-user-1/revoke", "other-key-0001", true, http.StatusBadRequest, false},
		{"capability not advertised", "/api/management/v2/tenants/tenant-a/members/cloud-user-1/revoke", "revoke-member-01", false, http.StatusNotFound, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &managementOperationServiceStub{accepted: operation}
			caps := allManagementCapabilities()
			caps[ManagementMemberRevoke] = test.enabled
			response := managementPost(managementOperationRouter(t, service, caps), test.path, body, test.header)
			if response.Code != test.want {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.want)
			}
			command, ok := service.command.(contract.RevokeTenantMember)
			if test.accepted != ok {
				t.Fatalf("accepted=%v command=%#v", ok, service.command)
			}
			if ok && (service.caller != "cloud-control-plane" || service.tenant != "tenant-a" || command.Subject != "cloud-user-1") {
				t.Fatalf("identity caller=%q tenant=%q command=%#v", service.caller, service.tenant, command)
			}
		})
	}
}

func TestManagementMemberRevokeAloneIsAValidCapabilitySet(t *testing.T) {
	service := &managementOperationServiceStub{accepted: management.Operation{ID: "operation-revoke-1", Tenant: "tenant-a", Caller: "cloud-control-plane", Kind: management.CommandTenantMemberRevoke, State: contract.OperationRequested, CreatedAt: time.Now(), UpdatedAt: time.Now()}}
	caps := map[ManagementCapability]bool{ManagementMemberRevoke: true, ManagementOperationRead: true}
	response := managementPost(managementOperationRouter(t, service, caps), "/api/management/v2/tenants/tenant-a/members/cloud-user-1/revoke", `{"meta":{"idempotency_key":"revoke-member-01"},"subject":"cloud-user-1","reason":"x"}`, "")
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
