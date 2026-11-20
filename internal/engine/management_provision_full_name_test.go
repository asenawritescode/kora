package engine

import (
	"testing"

	"github.com/asenawritescode/kora/contract"
)

// The owner's name Cloud sends becomes the bootstrap administrator's name;
// commands without it keep the "Administrator" default.
func TestBootstrapAdminFullName(t *testing.T) {
	for _, test := range []struct {
		name, fullName, want string
	}{
		{name: "owner name from Cloud", fullName: "Retail Owner", want: "Retail Owner"},
		{name: "surrounding space trimmed", fullName: "  Retail Owner  ", want: "Retail Owner"},
		{name: "legacy command without name", fullName: "", want: "Administrator"},
		{name: "blank name", fullName: "   ", want: "Administrator"},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := contract.ProvisionTenant{BootstrapAdminFullName: test.fullName}
			if got := bootstrapAdminFullName(command); got != test.want {
				t.Fatalf("bootstrapAdminFullName(%q) = %q, want %q", test.fullName, got, test.want)
			}
		})
	}
}
