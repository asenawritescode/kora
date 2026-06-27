package auth

import (
	"testing"

	"github.com/asenawritescode/kora/doctype"
)

func TestHasExtensionPermission(t *testing.T) {
	perms := []doctype.Permission{
		{Doctype: "Work Order", Read: true, Write: false, Create: true, Delete: false,
			Submit: false, Cancel: false, Amend: false, Export: true, Import: false, Report: false},
	}
	tests := []struct {
		name    string
		perms   []doctype.Permission
		doctype string
		op      string
		want    bool
	}{
		// Granted cases
		{"granted read on configured doctype", perms, "Work Order", "read", true},
		{"granted create on configured doctype", perms, "Work Order", "create", true},
		{"granted export on configured doctype", perms, "Work Order", "export", true},
		// Denied cases (configured but not granted)
		{"denied write on configured doctype", perms, "Work Order", "write", false},
		{"denied delete on configured doctype", perms, "Work Order", "delete", false},
		{"denied submit on configured doctype", perms, "Work Order", "submit", false},
		// Unconfigured doctype
		{"denied read on unconfigured doctype", perms, "Invoice", "read", false},
		{"denied write on unconfigured doctype", perms, "Invoice", "write", false},
		// Empty/nil permissions
		{"empty permissions deny all", []doctype.Permission{}, "Work Order", "read", false},
		{"nil permissions deny all", nil, "Work Order", "read", false},
		// Each of the 10 operations individually
		{"read only grants read", []doctype.Permission{{Doctype: "WO", Read: true}}, "WO", "read", true},
		{"read only denies write", []doctype.Permission{{Doctype: "WO", Read: true}}, "WO", "write", false},
		{"write only grants write", []doctype.Permission{{Doctype: "WO", Write: true}}, "WO", "write", true},
		{"create only grants create", []doctype.Permission{{Doctype: "WO", Create: true}}, "WO", "create", true},
		{"delete only grants delete", []doctype.Permission{{Doctype: "WO", Delete: true}}, "WO", "delete", true},
		{"submit only grants submit", []doctype.Permission{{Doctype: "WO", Submit: true}}, "WO", "submit", true},
		{"cancel only grants cancel", []doctype.Permission{{Doctype: "WO", Cancel: true}}, "WO", "cancel", true},
		{"amend only grants amend", []doctype.Permission{{Doctype: "WO", Amend: true}}, "WO", "amend", true},
		{"export only grants export", []doctype.Permission{{Doctype: "WO", Export: true}}, "WO", "export", true},
		{"import only grants import", []doctype.Permission{{Doctype: "WO", Import: true}}, "WO", "import", true},
		{"report only grants report", []doctype.Permission{{Doctype: "WO", Report: true}}, "WO", "report", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HasExtensionPermission(tt.perms, tt.doctype, tt.op)
			if got != tt.want {
				t.Errorf("HasExtensionPermission(%v, %q, %q) = %v, want %v",
					tt.perms, tt.doctype, tt.op, got, tt.want)
			}
		})
	}
}
