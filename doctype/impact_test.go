package doctype_test

import (
	"testing"

	"github.com/asenawritescode/kora/doctype"
)

func TestAnalyzeImpactFromChanges_Tiers(t *testing.T) {
	tests := []struct {
		name     string
		changes  []doctype.Change
		wantTier doctype.ImpactTier
	}{
		{
			name: "add-doctype is safe",
			changes: []doctype.Change{
				{Type: "add-doctype", Section: "doctypes", Entity: "Product"},
			},
			wantTier: doctype.TierSafe,
		},
		{
			name: "remove-doctype is warning",
			changes: []doctype.Change{
				{Type: "remove-doctype", Section: "doctypes", Entity: "Product"},
			},
			wantTier: doctype.TierWarning,
		},
		{
			name: "add-field with defaults is safe",
			changes: []doctype.Change{
				{
					Type: "add-field", Section: "doctypes", Entity: "Product", Field: "title",
					Attrs: map[string]any{":type": "Data", ":required": false},
				},
			},
			wantTier: doctype.TierSafe,
		},
		{
			name: "add-field required no default is warning",
			changes: []doctype.Change{
				{
					Type: "add-field", Section: "doctypes", Entity: "Product", Field: "title",
					Attrs: map[string]any{":type": "Data", ":required": true},
				},
			},
			wantTier: doctype.TierWarning,
		},
		{
			name: "add-field required with default is safe",
			changes: []doctype.Change{
				{
					Type: "add-field", Section: "doctypes", Entity: "Product", Field: "title",
					Attrs: map[string]any{":type": "Data", ":required": true, ":default": "untitled"},
				},
			},
			wantTier: doctype.TierSafe,
		},
		{
			name: "remove-field is warning",
			changes: []doctype.Change{
				{Type: "remove-field", Section: "doctypes", Entity: "Product", Field: "old_field"},
			},
			wantTier: doctype.TierWarning,
		},
		{
			name: "rename-field is safe",
			changes: []doctype.Change{
				{
					Type: "rename-field", Section: "doctypes", Entity: "Product", Field: "new_name",
					Attrs: map[string]any{":renamed-from": "old_name"},
				},
			},
			wantTier: doctype.TierSafe,
		},
		{
			name: "change-field-type is blocked",
			changes: []doctype.Change{
				{
					Type: "change-field-type", Section: "doctypes", Entity: "Product", Field: "price",
					Attrs: map[string]any{":old_type": "Int", ":type": "Data"},
				},
			},
			wantTier: doctype.TierBlocked,
		},
		{
			name: "change-field-property making required is warning",
			changes: []doctype.Change{
				{
					Type: "change-field-property", Section: "doctypes", Entity: "Product", Field: "title",
					Attrs: map[string]any{":required": true},
				},
			},
			wantTier: doctype.TierWarning,
		},
		{
			name: "change-field-property adding constraints is warning",
			changes: []doctype.Change{
				{
					Type: "change-field-property", Section: "doctypes", Entity: "Product", Field: "age",
					Attrs: map[string]any{":constraints": []string{"min"}},
				},
			},
			wantTier: doctype.TierWarning,
		},
		{
			name: "change-field-property other change is safe",
			changes: []doctype.Change{
				{
					Type: "change-field-property", Section: "doctypes", Entity: "Product", Field: "title",
					Attrs: map[string]any{":old_value": "false"},
				},
			},
			wantTier: doctype.TierSafe,
		},
		{
			name: "add-role is safe",
			changes: []doctype.Change{
				{Type: "add-role", Section: "roles", Entity: "Manager"},
			},
			wantTier: doctype.TierSafe,
		},
		{
			name: "remove-role is warning",
			changes: []doctype.Change{
				{Type: "remove-role", Section: "roles", Entity: "Manager"},
			},
			wantTier: doctype.TierWarning,
		},
		{
			name: "add-perm is safe",
			changes: []doctype.Change{
				{Type: "add-perm", Section: "permissions", Entity: "Product|Manager"},
			},
			wantTier: doctype.TierSafe,
		},
		{
			name: "remove-perm is safe",
			changes: []doctype.Change{
				{Type: "remove-perm", Section: "permissions", Entity: "Product|Manager"},
			},
			wantTier: doctype.TierSafe,
		},
		{
			name: "add-workflow is safe",
			changes: []doctype.Change{
				{Type: "add-workflow", Section: "workflows", Entity: "Product"},
			},
			wantTier: doctype.TierSafe,
		},
		{
			name: "remove-workflow is warning",
			changes: []doctype.Change{
				{Type: "remove-workflow", Section: "workflows", Entity: "Product"},
			},
			wantTier: doctype.TierWarning,
		},
		{
			name: "add-metric is safe",
			changes: []doctype.Change{
				{Type: "add-metric", Section: "analytics_metrics", Entity: "total_sales"},
			},
			wantTier: doctype.TierSafe,
		},
		{
			name: "remove-metric is safe",
			changes: []doctype.Change{
				{Type: "remove-metric", Section: "analytics_metrics", Entity: "total_sales"},
			},
			wantTier: doctype.TierSafe,
		},
		{
			name: "add-script-ref is safe",
			changes: []doctype.Change{
				{Type: "add-script-ref", Section: "scripts", Entity: "validate_order"},
			},
			wantTier: doctype.TierSafe,
		},
		{
			name: "remove-script-ref is safe",
			changes: []doctype.Change{
				{Type: "remove-script-ref", Section: "scripts", Entity: "validate_order"},
			},
			wantTier: doctype.TierSafe,
		},
		{
			name: "unknown change type is warning",
			changes: []doctype.Change{
				{Type: "unknown-type", Section: "doctypes"},
			},
			wantTier: doctype.TierWarning,
		},
		{
			name: "mixed tiers picks worst (blocked > warning > safe)",
			changes: []doctype.Change{
				{Type: "add-doctype", Section: "doctypes", Entity: "Product"},
				{
					Type: "change-field-type", Section: "doctypes", Entity: "Product", Field: "price",
					Attrs: map[string]any{":old_type": "Int", ":type": "Data"},
				},
			},
			wantTier: doctype.TierBlocked,
		},
		{
			name:     "empty changes returns safe",
			changes:  []doctype.Change{},
			wantTier: doctype.TierSafe,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := doctype.AnalyzeImpactFromChanges(tt.changes)
			if result.Tier != tt.wantTier {
				t.Errorf("AnalyzeImpactFromChanges().Tier = %q, want %q", result.Tier, tt.wantTier)
			}
			if result.Summary == "" {
				t.Error("AnalyzeImpactFromChanges().Summary should not be empty")
			}
		})
	}
}

func TestAnalyzeImpactFromChanges_ChangesPreserved(t *testing.T) {
	changes := []doctype.Change{
		{Type: "add-doctype", Section: "doctypes", Entity: "Product"},
		{Type: "remove-field", Section: "doctypes", Entity: "Product", Field: "old_field"},
	}
	result := doctype.AnalyzeImpactFromChanges(changes)
	if len(result.Changes) != len(changes) {
		t.Errorf("expected %d changes, got %d", len(changes), len(result.Changes))
	}
	for i := range changes {
		if result.Changes[i].Type != changes[i].Type {
			t.Errorf("change %d: type = %q, want %q", i, result.Changes[i].Type, changes[i].Type)
		}
	}
}
