package contract

import (
	"strings"
	"testing"
)

func TestValidatePlanTenantMember(t *testing.T) {
	valid := PlanTenantMember{Meta: CommandMeta{IdempotencyKey: "member-plan-1"}, Email: "member@example.test", Roles: []string{"Front Desk"}}
	tests := []struct {
		name   string
		mutate func(*PlanTenantMember)
		valid  bool
	}{
		{name: "valid", mutate: func(*PlanTenantMember) {}, valid: true},
		{name: "missing idempotency key", mutate: func(c *PlanTenantMember) { c.Meta.IdempotencyKey = "" }},
		{name: "display name mailbox", mutate: func(c *PlanTenantMember) { c.Email = "Member <member@example.test>" }},
		{name: "two mailboxes", mutate: func(c *PlanTenantMember) { c.Email = "a@example.test,b@example.test" }},
		{name: "trailing newline", mutate: func(c *PlanTenantMember) { c.Email = "member@example.test\n" }},
		{name: "oversized email", mutate: func(c *PlanTenantMember) { c.Email = strings.Repeat("a", 250) + "@example.test" }},
		{name: "no role", mutate: func(c *PlanTenantMember) { c.Roles = nil }},
		{name: "two roles", mutate: func(c *PlanTenantMember) { c.Roles = []string{"Front Desk", "Stock"} }},
		{name: "blank role", mutate: func(c *PlanTenantMember) { c.Roles = []string{" "} }},
		{name: "padded role", mutate: func(c *PlanTenantMember) { c.Roles = []string{" Front Desk"} }},
		{name: "control character role", mutate: func(c *PlanTenantMember) { c.Roles = []string{"Front\x00Desk"} }},
		{name: "oversized role", mutate: func(c *PlanTenantMember) { c.Roles = []string{strings.Repeat("r", 141)} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := valid
			command.Roles = append([]string(nil), valid.Roles...)
			test.mutate(&command)
			if err := ValidatePlanTenantMember(command); (err == nil) != test.valid {
				t.Fatalf("ValidatePlanTenantMember(%+v) error = %v, valid=%v", command, err, test.valid)
			}
		})
	}
}

func TestValidateGrantTenantMember(t *testing.T) {
	valid := GrantTenantMember{Meta: CommandMeta{IdempotencyKey: "inv_01K7INVITATION"}, InvitationID: "inv_01K7INVITATION", Subject: "usr_member_1", Email: "member@example.test", FullName: "Amina Otieno", Roles: []string{"Front Desk"}}
	tests := []struct {
		name   string
		mutate func(*GrantTenantMember)
		valid  bool
	}{
		{name: "valid", mutate: func(*GrantTenantMember) {}, valid: true},
		{name: "non-latin full name", mutate: func(c *GrantTenantMember) { c.FullName = "Zoë Ağaoğlu" }, valid: true},
		{name: "idempotency key differs from invitation", mutate: func(c *GrantTenantMember) { c.Meta.IdempotencyKey = "other-key-1" }},
		{name: "missing invitation", mutate: func(c *GrantTenantMember) { c.InvitationID, c.Meta.IdempotencyKey = "", "" }},
		{name: "missing subject", mutate: func(c *GrantTenantMember) { c.Subject = "" }},
		{name: "short subject", mutate: func(c *GrantTenantMember) { c.Subject = "ab" }},
		{name: "padded subject", mutate: func(c *GrantTenantMember) { c.Subject = " usr_member_1" }},
		{name: "control character subject", mutate: func(c *GrantTenantMember) { c.Subject = "usr\nmember" }},
		{name: "subject with path separator", mutate: func(c *GrantTenantMember) { c.Subject = "usr/member" }},
		{name: "subject with dot segment", mutate: func(c *GrantTenantMember) { c.Subject = "usr..member" }},
		{name: "subject longer than tenant user key", mutate: func(c *GrantTenantMember) { c.Subject = PrincipalID(strings.Repeat("s", 141)) }},
		{name: "invalid email", mutate: func(c *GrantTenantMember) { c.Email = "not-an-email" }},
		{name: "blank full name", mutate: func(c *GrantTenantMember) { c.FullName = "  " }},
		{name: "control character full name", mutate: func(c *GrantTenantMember) { c.FullName = "Amina\tOtieno" }},
		{name: "invalid utf-8 full name", mutate: func(c *GrantTenantMember) { c.FullName = "Amina \xff" }},
		{name: "oversized full name", mutate: func(c *GrantTenantMember) { c.FullName = strings.Repeat("n", 141) }},
		{name: "no role", mutate: func(c *GrantTenantMember) { c.Roles = []string{} }},
		{name: "two roles", mutate: func(c *GrantTenantMember) { c.Roles = []string{"Front Desk", "Stock"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := valid
			command.Roles = append([]string(nil), valid.Roles...)
			test.mutate(&command)
			if err := ValidateGrantTenantMember(command); (err == nil) != test.valid {
				t.Fatalf("ValidateGrantTenantMember(%+v) error = %v, valid=%v", command, err, test.valid)
			}
		})
	}
}

func TestValidateRevokeTenantMember(t *testing.T) {
	valid := RevokeTenantMember{Meta: CommandMeta{IdempotencyKey: "member-revoke-1"}, Subject: "usr_member_1", Reason: "owner removed the member from the team"}
	tests := []struct {
		name   string
		mutate func(*RevokeTenantMember)
		valid  bool
	}{
		{name: "valid", mutate: func(*RevokeTenantMember) {}, valid: true},
		{name: "missing idempotency key", mutate: func(c *RevokeTenantMember) { c.Meta.IdempotencyKey = "" }},
		{name: "missing subject", mutate: func(c *RevokeTenantMember) { c.Subject = "" }},
		{name: "subject with path separator", mutate: func(c *RevokeTenantMember) { c.Subject = "../usr" }},
		{name: "oversized subject", mutate: func(c *RevokeTenantMember) { c.Subject = PrincipalID(strings.Repeat("s", 141)) }},
		{name: "blank reason", mutate: func(c *RevokeTenantMember) { c.Reason = " \t" }},
		{name: "oversized reason", mutate: func(c *RevokeTenantMember) { c.Reason = strings.Repeat("x", 1001) }},
		{name: "invalid utf-8 reason", mutate: func(c *RevokeTenantMember) { c.Reason = "removed \xff" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := valid
			test.mutate(&command)
			if err := ValidateRevokeTenantMember(command); (err == nil) != test.valid {
				t.Fatalf("ValidateRevokeTenantMember(%+v) error = %v, valid=%v", command, err, test.valid)
			}
		})
	}
}

func TestValidateTenantMemberPlan(t *testing.T) {
	problem := func(code TenantMemberProblemCode) TenantMemberProblem {
		return TenantMemberProblem{Code: code, Message: "explained to the owner"}
	}
	tests := []struct {
		name  string
		value TenantMemberPlan
		valid bool
	}{
		{name: "allowed", value: TenantMemberPlan{Allowed: true}, valid: true},
		{name: "allowed with empty problems", value: TenantMemberPlan{Allowed: true, Problems: []TenantMemberProblem{}}, valid: true},
		{name: "unknown role", value: TenantMemberPlan{Problems: []TenantMemberProblem{problem(TenantMemberRoleUnknown)}}, valid: true},
		{name: "every known code", value: TenantMemberPlan{Problems: []TenantMemberProblem{problem(TenantMemberRoleUnknown), problem(TenantMemberRoleNotAssignable), problem(TenantMemberAlreadyExists), problem(TenantMemberEmailInUse)}}, valid: true},
		{name: "allowed with problem", value: TenantMemberPlan{Allowed: true, Problems: []TenantMemberProblem{problem(TenantMemberEmailInUse)}}},
		{name: "refused without problem", value: TenantMemberPlan{}},
		{name: "unknown code", value: TenantMemberPlan{Problems: []TenantMemberProblem{problem("member.unknown")}}},
		{name: "blank message", value: TenantMemberPlan{Problems: []TenantMemberProblem{{Code: TenantMemberRoleUnknown}}}},
		{name: "oversized message", value: TenantMemberPlan{Problems: []TenantMemberProblem{{Code: TenantMemberRoleUnknown, Message: strings.Repeat("m", 501)}}}},
		{name: "too many problems", value: TenantMemberPlan{Problems: []TenantMemberProblem{problem(TenantMemberRoleUnknown), problem(TenantMemberRoleUnknown), problem(TenantMemberRoleUnknown), problem(TenantMemberRoleUnknown), problem(TenantMemberRoleUnknown), problem(TenantMemberRoleUnknown), problem(TenantMemberRoleUnknown), problem(TenantMemberRoleUnknown), problem(TenantMemberRoleUnknown)}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateTenantMemberPlan(test.value); (err == nil) != test.valid {
				t.Fatalf("ValidateTenantMemberPlan(%+v) error = %v, valid=%v", test.value, err, test.valid)
			}
		})
	}
}

func TestManagementCapabilitiesAcceptTenantMemberOperations(t *testing.T) {
	value := ManagementCapabilitiesV2{Version: 2, Capabilities: []ManagementCapability{ManagementCapabilityOperationRead, ManagementCapabilityTenantMemberPlan, ManagementCapabilityTenantMemberGrant, ManagementCapabilityTenantMemberRevoke}}
	if err := ValidateManagementCapabilitiesV2(value); err != nil {
		t.Fatal(err)
	}
	if ManagementCapabilityTenantMemberPlan != "tenant.member.plan" || ManagementCapabilityTenantMemberGrant != "tenant.member.grant" || ManagementCapabilityTenantMemberRevoke != "tenant.member.revoke" {
		t.Fatal("tenant member capability names changed")
	}
}
