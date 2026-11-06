package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProvisionTenantDisplayName(t *testing.T) {
	base := ProvisionTenant{Meta: CommandMeta{IdempotencyKey: "provision-key-1"}, Tenant: "tenant-1", BootstrapAdminEmail: "admin@tenant.example.test", BootstrapAdminSubject: "usr_test_subject_1", PrimaryAlias: "dbg-duka-d6f2364e047c7286.example.test", Deployment: "deploy-1", DatabaseAllocationRef: "mysql:allocation-1"}
	tests := []struct {
		name, input, want string
		wantErr           bool
	}{
		{name: "absent keeps hostname label", input: "", want: ""},
		{name: "typed business name", input: "Dbg Duka", want: "Dbg Duka"},
		{name: "surrounding whitespace is trimmed", input: "  Mama Njeri Duka \t", want: "Mama Njeri Duka"},
		{name: "non-Latin text", input: "Duka la Mama — ジ店 🛒", want: "Duka la Mama — ジ店 🛒"},
		{name: "exactly the cap", input: strings.Repeat("é", MaxTenantDisplayNameLength), want: strings.Repeat("é", MaxTenantDisplayNameLength)},
		{name: "over the cap", input: strings.Repeat("a", MaxTenantDisplayNameLength+1), wantErr: true},
		{name: "whitespace only", input: "   ", wantErr: true},
		{name: "newline", input: "Dbg\nDuka", wantErr: true},
		{name: "tab inside", input: "Dbg\tDuka", wantErr: true},
		{name: "NUL", input: "Dbg\x00Duka", wantErr: true},
		{name: "DEL", input: "Dbg\x7fDuka", wantErr: true},
		{name: "C1 control", input: "Dbg\u0085Duka", wantErr: true},
		{name: "bidi override", input: "Dbg‮akuD", wantErr: true},
		{name: "bidi isolate", input: "Dbg⁦Duka", wantErr: true},
		{name: "invalid UTF-8", input: "Dbg\xffDuka", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NormalizeTenantDisplayName(test.input)
			if (err != nil) != test.wantErr {
				t.Fatalf("NormalizeTenantDisplayName(%q) error = %v, wantErr %v", test.input, err, test.wantErr)
			}
			if err == nil && got != test.want {
				t.Fatalf("NormalizeTenantDisplayName(%q) = %q, want %q", test.input, got, test.want)
			}
			cmd := base
			cmd.DisplayName = test.input
			if err := ValidateProvisionTenant(cmd); (err != nil) != test.wantErr {
				t.Fatalf("ValidateProvisionTenant(display_name=%q) error = %v, wantErr %v", test.input, err, test.wantErr)
			}
		})
	}
}

func TestProvisionTenantDisplayNameWireCompatibility(t *testing.T) {
	// A Cloud built before display_name existed sends no key; the command
	// must still decode and validate, and an absent name must not be emitted.
	legacy := `{"meta":{"idempotency_key":"provision-key-1"},"tenant_id":"tenant-1","bootstrap_admin_email":"admin@tenant.example.test","bootstrap_admin_subject":"usr_test_subject_1","primary_alias":"tenant.example.test","aliases":null,"deployment_id":"deploy-1","database_allocation_ref":"mysql:allocation-1"}`
	var cmd ProvisionTenant
	if err := json.Unmarshal([]byte(legacy), &cmd); err != nil {
		t.Fatal("decode legacy provisioning command:", err)
	}
	if err := ValidateProvisionTenant(cmd); err != nil {
		t.Fatal("validate legacy provisioning command:", err)
	}
	encoded, err := json.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "display_name") {
		t.Fatalf("absent display name was serialized: %s", encoded)
	}
	cmd.DisplayName = "Dbg Duka"
	encoded, err = json.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"display_name":"Dbg Duka"`) {
		t.Fatalf("display name missing from wire form: %s", encoded)
	}
}
