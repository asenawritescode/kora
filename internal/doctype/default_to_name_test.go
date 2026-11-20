package doctype

import (
	"strings"
	"testing"
)

func TestDefaultToNameNeedsAPlainDataField(t *testing.T) {
	tests := []struct {
		name, field, wantErr string
	}{
		{"data field", "    fieldtype: Data\n", ""},
		{"not data", "    fieldtype: Int\n", "default_to_name needs a Data field"},
		{"with a default", "    fieldtype: Data\n    default: X\n", "default_to_name needs a Data field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := "name: Trip\nmodule: Fleet\nfields:\n  - fieldname: reference\n" + tt.field + "    default_to_name: true\n"
			dt, err := ParseYAML([]byte(yaml), "trip.yaml")
			if tt.wantErr == "" {
				if err != nil || !dt.GetField("reference").DefaultToName {
					t.Fatalf("ParseYAML = %v, %v; want the flag set", dt, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ParseYAML error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
