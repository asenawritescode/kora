package contract

import (
	"strings"
	"testing"
)

func TestManagementCommandsValidateOpaqueReferencesAndDeploymentAuthority(t *testing.T) {
	base := ProvisionTenant{Meta: CommandMeta{IdempotencyKey: "provision-key-1"}, Tenant: "tenant-1", BootstrapAdminEmail: "admin@tenant.example.test", BootstrapAdminSubject: "usr_test_subject_1", PrimaryAlias: "tenant.example.test", Deployment: "deploy-1", DatabaseAllocationRef: "mysql:allocation-1"}
	if err := ValidateProvisionTenant(base); err != nil {
		t.Fatal("valid provision command:", err)
	}
	for _, value := range []string{"mysql://user:pass@host/db", "/var/lib/kora/tenant.db", "../secret", "db name"} {
		cmd := base
		cmd.DatabaseAllocationRef = value
		if err := ValidateProvisionTenant(cmd); err == nil {
			t.Errorf("accepted unsafe database allocation reference %q", value)
		}
	}
	cmd := base
	cmd.Aliases = []string{"alias.tenant.example.test", "alias.tenant.example.test"}
	if err := ValidateProvisionTenant(cmd); err == nil {
		t.Fatal("accepted duplicate aliases")
	}
	cmd = base
	cmd.PrimaryAlias = "Tenant.example.test"
	if err := ValidateProvisionTenant(cmd); err == nil {
		t.Fatal("accepted noncanonical primary hostname")
	}
	cmd = base
	cmd.Aliases = []string{base.PrimaryAlias}
	if err := ValidateProvisionTenant(cmd); err == nil {
		t.Fatal("accepted primary hostname duplicated among aliases")
	}
	for _, email := range []string{"", "not-an-email", "Admin <admin@tenant.example.test>", "admin@tenant.example.test\n"} {
		cmd = base
		cmd.BootstrapAdminEmail = email
		if err := ValidateProvisionTenant(cmd); err == nil {
			t.Errorf("accepted invalid or noncanonical bootstrap email %q", email)
		}
	}
}

func TestRequestTenantInvitationValidatesSingleMailbox(t *testing.T) {
	command := RequestTenantInvitation{Meta: CommandMeta{IdempotencyKey: "invitation-key-1"}, Email: "owner@example.test"}
	if err := ValidateRequestTenantInvitation(command); err != nil {
		t.Fatal("valid invitation command:", err)
	}
	for _, email := range []string{"", "x", "Owner <owner@example.test>", "owner@example.test,other@example.test", "owner@example.test\n"} {
		command.Email = email
		if err := ValidateRequestTenantInvitation(command); err == nil {
			t.Errorf("accepted invalid/noncanonical invitation mailbox %q", email)
		}
	}
}

func TestManagementCommandsRequireExpectedRevisionAndConstrainedState(t *testing.T) {
	valid := ApplyPackage{Meta: CommandMeta{IdempotencyKey: "apply-key-1"}, Tenant: "tenant-1", ArtifactRef: "object:release-1", Digest: strings.Repeat("a", 64), ExpectedConfigRevision: "4"}
	if err := ValidateApplyPackage(valid); err != nil {
		t.Fatal("valid package command:", err)
	}
	valid.OptionalReleaseIDs = []string{"team-release-1.0.0", "reports-release-1.0.0"}
	if err := ValidateApplyPackage(valid); err != nil {
		t.Fatal("valid selected package components:", err)
	}
	valid.ProviderSelections = []ProviderSelection{{Key: BindingKey{Scope: BusinessScope{TenantID: valid.Tenant, Kind: ScopeKindTenant}, ConsumerModuleID: "sales.orders", CapabilityName: "billing.issue"}, ProviderID: "billing-external"}}
	if err := ValidateApplyPackage(valid); err != nil {
		t.Fatal("valid provider selection:", err)
	}
	valid.ProviderSelections[0].Key.Scope.TenantID = "tenant-2"
	if err := ValidateApplyPackage(valid); err == nil {
		t.Fatal("accepted cross-tenant provider selection")
	}
	valid.ProviderSelections[0].Key.Scope.TenantID = valid.Tenant
	valid.ProviderSelections = append(valid.ProviderSelections, valid.ProviderSelections[0])
	if err := ValidateApplyPackage(valid); err == nil {
		t.Fatal("accepted duplicate provider selection")
	}
	valid.ProviderSelections = nil
	valid.OptionalReleaseIDs = []string{"team-release-1.0.0", "team-release-1.0.0"}
	if err := ValidateApplyPackage(valid); err == nil {
		t.Fatal("accepted duplicate package component release IDs")
	}
	valid.OptionalReleaseIDs = nil
	valid.ExpectedConfigRevision = ""
	if err := ValidateApplyPackage(valid); err == nil {
		t.Fatal("accepted package command without revision binding")
	}
	if err := ValidateSetTenantState(SetTenantState{Meta: CommandMeta{IdempotencyKey: "state-key-1"}, State: TenantDeleting, ExpectedConfigRevision: "1", Reason: "user request"}); err == nil {
		t.Fatal("allowed Cloud to request a lifecycle outcome state")
	}
	if err := ValidateSetTenantState(SetTenantState{Meta: CommandMeta{IdempotencyKey: "state-key-1"}, State: TenantSuspended, Reason: "user request"}); err == nil {
		t.Fatal("accepted state command without config revision binding")
	}
	if err := ValidateDeleteTenant(DeleteTenant{Meta: CommandMeta{IdempotencyKey: "delete-key-1"}}); err == nil {
		t.Fatal("accepted deletion without recovery checkpoint")
	}
}

func TestPackageReconciliationRequiresReasonAndStableKey(t *testing.T) {
	valid := ReconcileTenantPackageOperation{
		Meta:   CommandMeta{IdempotencyKey: "reconcile-package-1"},
		Reason: "operator reviewed the durable activation journal",
	}
	if err := ValidateReconcileTenantPackageOperation(valid); err != nil {
		t.Fatal("valid package reconciliation:", err)
	}
	valid.Reason = "  "
	if err := ValidateReconcileTenantPackageOperation(valid); err == nil {
		t.Fatal("accepted package reconciliation without an operator reason")
	}
}

func TestManagementProjectionVersionAndOperationStateAreChecked(t *testing.T) {
	descriptor := TenantDescriptorV2{Version: 2, SiteID: "tenant-1", Aliases: []string{"tenant.example.test"}, Status: TenantActive, ConfigRevision: "4", DirectoryRevision: "9", Deployment: "deployment-a", DataPlaneRef: "mysql:pool-a"}
	if err := ValidateTenantDescriptorV2(descriptor); err != nil {
		t.Fatal("valid descriptor:", err)
	}
	descriptor.TenantConfigRevision = "12"
	if err := ValidateTenantDescriptorV2(descriptor); err != nil {
		t.Fatal("valid descriptor with separate tenant config revision:", err)
	}
	descriptor.TenantConfigRevision = "01"
	if err := ValidateTenantDescriptorV2(descriptor); err == nil {
		t.Fatal("accepted non-canonical tenant config revision")
	}
	descriptor.TenantConfigRevision = ""
	descriptor.Version = 1
	if err := ValidateTenantDescriptorV2(descriptor); err == nil {
		t.Fatal("accepted stale descriptor version")
	}
	descriptor.Version = 2
	descriptor.Deployment = ""
	if err := ValidateTenantDescriptorV2(descriptor); err == nil {
		t.Fatal("accepted descriptor without authoritative deployment assignment")
	}
	operation := ManagedOperation{ID: "operation-1", Tenant: "tenant-1", State: OperationRecoveryRequired, ObservedRevision: "9"}
	if err := ValidateManagedOperation(operation); err != nil {
		t.Fatal("valid operation:", err)
	}
	operation.State = "done"
	if err := ValidateManagedOperation(operation); err == nil {
		t.Fatal("accepted unknown operation state")
	}
}

func TestManagementCapabilitiesV2Validation(t *testing.T) {
	tests := []struct {
		name  string
		value ManagementCapabilitiesV2
		valid bool
	}{
		{name: "supported subset", value: ManagementCapabilitiesV2{Version: 2, Capabilities: []ManagementCapability{ManagementCapabilityOperationRead, ManagementCapabilityTenantProvision, ManagementCapabilityTenantProvisionRetry}}, valid: true},
		{name: "wrong version", value: ManagementCapabilitiesV2{Version: 1, Capabilities: []ManagementCapability{ManagementCapabilityOperationRead}}},
		{name: "missing operation read", value: ManagementCapabilitiesV2{Version: 2, Capabilities: []ManagementCapability{ManagementCapabilityTenantProvision}}},
		{name: "unknown operation", value: ManagementCapabilitiesV2{Version: 2, Capabilities: []ManagementCapability{ManagementCapabilityOperationRead, "tenant.unknown"}}},
		{name: "duplicate operation", value: ManagementCapabilitiesV2{Version: 2, Capabilities: []ManagementCapability{ManagementCapabilityOperationRead, ManagementCapabilityOperationRead}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateManagementCapabilitiesV2(test.value)
			if (err == nil) != test.valid {
				t.Fatalf("ValidateManagementCapabilitiesV2(%+v) error = %v, valid=%v", test.value, err, test.valid)
			}
		})
	}
}

func TestEngineVersionAtLeast(t *testing.T) {
	tests := []struct {
		running, required string
		want              bool
		wantErr           bool
	}{
		{running: "1.2.3", required: "1.2.3", want: true},
		{running: "v1.3.0", required: "1.2.9", want: true},
		{running: "1.2.2", required: "1.2.3", want: false},
		{running: "1.0", required: "1.0.0", wantErr: true},
		{running: "1.0.0-alpha", required: "1.0.0", wantErr: true},
	}
	for _, tc := range tests {
		got, err := EngineVersionAtLeast(tc.running, tc.required)
		if (err != nil) != tc.wantErr || (err == nil && got != tc.want) {
			t.Errorf("EngineVersionAtLeast(%q, %q) = %v, %v", tc.running, tc.required, got, err)
		}
	}
}

func TestTenantStateReconciliationRequiresIdempotencyAndReason(t *testing.T) {
	command := ReconcileTenantStateOperation{
		Meta:   CommandMeta{IdempotencyKey: "reconcile-state-1"},
		Reason: "operator verified the current directory state",
	}
	if err := ValidateReconcileTenantStateOperation(command); err != nil {
		t.Fatal("valid reconciliation command:", err)
	}
	command.Meta.IdempotencyKey = ""
	if err := ValidateReconcileTenantStateOperation(command); err == nil {
		t.Fatal("accepted reconciliation without idempotency key")
	}
	command.Meta.IdempotencyKey = "reconcile-state-1"
	command.Reason = " \t"
	if err := ValidateReconcileTenantStateOperation(command); err == nil {
		t.Fatal("accepted reconciliation without a meaningful reason")
	}
	command.Reason = strings.Repeat("x", 8193)
	if err := ValidateReconcileTenantStateOperation(command); err == nil {
		t.Fatal("accepted oversized reconciliation reason")
	}
}
