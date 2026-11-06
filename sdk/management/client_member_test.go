package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestClientTenantMemberRoundTrips(t *testing.T) {
	tenant := contract.TenantID("tenant-a")
	operationID := contract.OperationID(contract.NewID())
	plan := contract.PlanTenantMember{Meta: contract.CommandMeta{IdempotencyKey: "member-plan-1"}, Email: "member@example.test", Roles: []string{"Front Desk"}}
	grant := contract.GrantTenantMember{Meta: contract.CommandMeta{IdempotencyKey: "inv_01K7INVITATION"}, InvitationID: "inv_01K7INVITATION", Subject: "usr_01:member", Email: "member@example.test", FullName: "Amina Otieno", Roles: []string{"Front Desk"}}
	revoke := contract.RevokeTenantMember{Meta: contract.CommandMeta{IdempotencyKey: "member-revoke-1"}, Subject: "usr_01:member", Reason: "owner removed the member from the team"}
	tests := []struct {
		name       string
		path       string
		status     int
		response   any
		call       func(Client) (any, error)
		wantErr    bool
		wantDecode any
	}{
		{name: "plan allowed", path: "/api/management/v2/tenants/tenant-a/members/plan", status: http.StatusOK, response: contract.TenantMemberPlan{Allowed: true}, wantDecode: &contract.PlanTenantMember{}, call: func(c Client) (any, error) { return c.PlanTenantMember(context.Background(), tenant, plan) }},
		{name: "plan refused", path: "/api/management/v2/tenants/tenant-a/members/plan", status: http.StatusOK, response: contract.TenantMemberPlan{Problems: []contract.TenantMemberProblem{{Code: contract.TenantMemberRoleNotAssignable, Message: "this role cannot be assigned by invitation"}}}, wantDecode: &contract.PlanTenantMember{}, call: func(c Client) (any, error) { return c.PlanTenantMember(context.Background(), tenant, plan) }},
		{name: "plan inconsistent response", path: "/api/management/v2/tenants/tenant-a/members/plan", status: http.StatusOK, response: contract.TenantMemberPlan{Allowed: true, Problems: []contract.TenantMemberProblem{{Code: contract.TenantMemberEmailInUse, Message: "email belongs to another user"}}}, wantErr: true, call: func(c Client) (any, error) { return c.PlanTenantMember(context.Background(), tenant, plan) }},
		{name: "grant", path: "/api/management/v2/tenants/tenant-a/members", status: http.StatusAccepted, response: contract.ManagedOperation{ID: operationID, Tenant: tenant, State: contract.OperationRequested}, wantDecode: &contract.GrantTenantMember{}, call: func(c Client) (any, error) { return c.GrantTenantMember(context.Background(), tenant, grant) }},
		{name: "grant for another tenant", path: "/api/management/v2/tenants/tenant-a/members", status: http.StatusAccepted, response: contract.ManagedOperation{ID: operationID, Tenant: "tenant-b", State: contract.OperationRequested}, wantErr: true, call: func(c Client) (any, error) { return c.GrantTenantMember(context.Background(), tenant, grant) }},
		{name: "revoke", path: "/api/management/v2/tenants/tenant-a/members/usr_01:member/revoke", status: http.StatusAccepted, response: contract.ManagedOperation{ID: operationID, Tenant: tenant, State: contract.OperationRequested}, wantDecode: &contract.RevokeTenantMember{}, call: func(c Client) (any, error) { return c.RevokeTenantMember(context.Background(), tenant, revoke) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.EscapedPath() != test.path {
					t.Errorf("request = %s %s, want POST %s", r.Method, r.URL.EscapedPath(), test.path)
				}
				if test.wantDecode != nil {
					decoder := json.NewDecoder(r.Body)
					decoder.DisallowUnknownFields()
					if err := decoder.Decode(test.wantDecode); err != nil {
						t.Errorf("decode request: %v", err)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": test.response})
			}))
			defer server.Close()
			client, err := NewClient(Config{BaseURL: server.URL, Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "token", nil })})
			if err != nil {
				t.Fatal(err)
			}
			result, err := test.call(client)
			if (err != nil) != test.wantErr {
				t.Fatalf("result = %+v, error = %v, wantErr=%v", result, err, test.wantErr)
			}
			switch decoded := test.wantDecode.(type) {
			case *contract.PlanTenantMember:
				if decoded.Email != plan.Email || len(decoded.Roles) != 1 || decoded.Roles[0] != plan.Roles[0] {
					t.Fatalf("server decoded plan request %+v", decoded)
				}
			case *contract.GrantTenantMember:
				if decoded.InvitationID != grant.InvitationID || decoded.Subject != grant.Subject || decoded.FullName != grant.FullName {
					t.Fatalf("server decoded grant request %+v", decoded)
				}
			case *contract.RevokeTenantMember:
				if decoded.Subject != revoke.Subject || decoded.Reason != revoke.Reason {
					t.Fatalf("server decoded revoke request %+v", decoded)
				}
			}
		})
	}
}

func TestClientValidatesTenantMemberCommandsBeforeSending(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "token", nil })})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	validPlan := contract.PlanTenantMember{Meta: contract.CommandMeta{IdempotencyKey: "member-plan-1"}, Email: "member@example.test", Roles: []string{"Front Desk"}}
	if _, err := client.PlanTenantMember(ctx, "", validPlan); err == nil {
		t.Fatal("plan accepted an empty tenant")
	}
	if _, err := client.PlanTenantMember(ctx, "tenant-a", contract.PlanTenantMember{Meta: validPlan.Meta, Email: validPlan.Email, Roles: []string{"Front Desk", "Stock"}}); err == nil {
		t.Fatal("plan accepted two roles")
	}
	if _, err := client.GrantTenantMember(ctx, "tenant-a", contract.GrantTenantMember{}); err == nil {
		t.Fatal("grant accepted an empty command")
	}
	if _, err := client.RevokeTenantMember(ctx, "tenant-a", contract.RevokeTenantMember{Meta: contract.CommandMeta{IdempotencyKey: "member-revoke-1"}, Subject: "usr_member_1"}); err == nil {
		t.Fatal("revoke accepted a missing reason")
	}
	if calls != 0 {
		t.Fatalf("invalid member commands made %d HTTP calls", calls)
	}
}
