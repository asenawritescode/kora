package kernel

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
)

func TestActingRoles(t *testing.T) {
	tests := []struct {
		name     string
		roles    []string
		userRole string
		want     []string
	}{
		{"no roles", nil, "", nil},
		{"roles only", []string{"Sales", "Manager"}, "", []string{"Sales", "Manager"}},
		{"primary role already listed", []string{"Sales", "Manager"}, "Manager", []string{"Sales", "Manager"}},
		{"primary role first when missing", []string{"Sales"}, "Manager", []string{"Manager", "Sales"}},
		{"primary role alone", nil, "Manager", []string{"Manager"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			op := Operation{Context: OperationContext{Roles: tc.roles, UserRole: tc.userRole}}
			if got := actingRoles(op); !reflect.DeepEqual(got, tc.want) && (len(got) != 0 || len(tc.want) != 0) {
				t.Fatalf("actingRoles = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPublicFormDataKeepsOnlyPublishedFields(t *testing.T) {
	dt := &doctype.DocType{Name: "Lead", PublicAccess: &doctype.PublicAccess{Fields: []string{"name"}}}
	tests := []struct {
		name    string
		raw     string
		want    map[string]any
		wantErr bool
	}{
		{"published field kept", `{"name":"Ada","internal_note":"x"}`, map[string]any{"name": "Ada"}, false},
		{"nothing published", `{"internal_note":"x"}`, map[string]any{}, false},
		{"not an object", `["name"]`, nil, true},
	}
	m := &mutation{dt: dt}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, cerr := m.publicFormData(json.RawMessage(tc.raw))
			if tc.wantErr {
				if cerr == nil || cerr.Type != contract.CodeValidationFailed {
					t.Fatalf("err = %v, want validation failure", cerr)
				}
				return
			}
			if cerr != nil {
				t.Fatal(cerr)
			}
			var decoded map[string]any
			if err := json.Unmarshal(got, &decoded); err != nil || !reflect.DeepEqual(decoded, tc.want) {
				t.Fatalf("data = %s, want %v", got, tc.want)
			}
		})
	}
}
