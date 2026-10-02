package doctype

import "testing"

func TestNormalizeResourceName(t *testing.T) {
	tests := []struct {
		name        string
		resource    string
		displayName string
		want        string
	}{
		{name: "from display name", displayName: "Resource Name X", want: "resource-name-x"},
		{name: "keeps explicit value", resource: "ResourceNameX", displayName: "Ignored", want: "resourcenamex"},
		{name: "collapses punctuation", displayName: "Resource / Name X", want: "resource-name-x"},
		{name: "fallback", displayName: "!!!", want: "doctype"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeResourceName(tc.resource, tc.displayName); got != tc.want {
				t.Fatalf("normalizeResourceName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateAssignsResourceName(t *testing.T) {
	dt := &DocType{Name: "Resource Name X", Module: "Test"}
	if err := dt.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if dt.ResourceName != "resource-name-x" {
		t.Fatalf("ResourceName = %q, want %q", dt.ResourceName, "resource-name-x")
	}
	if dt.TableName() != "`tabResource Name X`" {
		t.Fatalf("TableName() = %q", dt.TableName())
	}
}

func TestRegistryResolvesDashedAliasForLegacyResourceName(t *testing.T) {
	registry := NewRegistry()
	dt := &DocType{Name: "Work Order", ResourceName: "work_order", Module: "Operations"}
	registry.Register(dt)
	if got := registry.Get("work-order"); got != dt {
		t.Fatalf("dashed API alias did not resolve legacy DocType: %#v", got)
	}
	if got := registry.Get("work_order"); got != dt {
		t.Fatalf("legacy resource name no longer resolves: %#v", got)
	}
}

func TestValidateAllRejectsDuplicateResourceNames(t *testing.T) {
	doctypes := []*DocType{
		{Name: "Work Order", Module: "Operations"},
		{Name: "Work-Order", Module: "Legacy"},
	}
	errs := ValidateAll(doctypes)
	for _, err := range errs {
		if err.Code == "DuplicateResourceName" {
			return
		}
	}
	t.Fatalf("expected DuplicateResourceName, got %#v", errs)
}
