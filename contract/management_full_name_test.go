package contract

import (
	"strings"
	"testing"
)

// BootstrapAdminFullName is optional: absent or blank passes, while an
// over-long value is rejected by the validator before Engine state changes.
func TestValidateProvisionTenantBootstrapAdminFullName(t *testing.T) {
	base := ProvisionTenant{Meta: CommandMeta{IdempotencyKey: "provision-key-1"}, Tenant: "tenant-1", BootstrapAdminEmail: "owner@tenant.example.test", BootstrapAdminSubject: "usr_test_subject_1", PrimaryAlias: "tenant.example.test", Deployment: "deploy-1", DatabaseAllocationRef: "mysql:allocation-1"}
	for _, test := range []struct {
		name     string
		fullName string
		wantErr  bool
	}{
		{name: "absent", fullName: ""},
		{name: "owner name", fullName: "Retail Owner"},
		{name: "longest allowed", fullName: strings.Repeat("a", MaxBootstrapAdminFullNameBytes)},
		{name: "too long", fullName: strings.Repeat("a", MaxBootstrapAdminFullNameBytes+1), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd := base
			cmd.BootstrapAdminFullName = test.fullName
			err := ValidateProvisionTenant(cmd)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateProvisionTenant(%q) error = %v, wantErr %v", test.fullName, err, test.wantErr)
			}
		})
	}
}
