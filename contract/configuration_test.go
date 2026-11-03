package contract

import "testing"

func TestConfigurationSurfaceBindingValidation(t *testing.T) {
	scope := BusinessScope{TenantID: "tenant-a", Kind: ScopeKindTenant}
	valid := []ConfigurationSurfaceBinding{
		{Kind: ConfigurationQuery, ViewName: "Invoice List", CatalogRevision: 3, BusinessScope: scope, OwnerModuleID: "sales.orders", Name: "orders.list", InputSchema: &SchemaRef{ID: "sales.orders.input", Version: 1}, OutputSchema: SchemaRef{ID: "sales.orders.output", Version: 2}},
		{Kind: ConfigurationProjection, ViewName: "Invoice History", CatalogRevision: 3, BusinessScope: scope, OwnerModuleID: "sales.orders", Name: "sales.history", OutputSchema: SchemaRef{ID: "sales.history", Version: 1}},
		{Kind: ConfigurationOperation, ViewName: "Invoice Detail", CatalogRevision: 3, BusinessScope: scope, OwnerModuleID: "sales.orders", OperationRef: PublishedOperationRef("sales.orders", "sales.invoice", "issue"), InputSchema: &SchemaRef{ID: "sales.invoice.issue.input", Version: 1}, OutputSchema: SchemaRef{ID: "sales.invoice.issue.output", Version: 1}},
	}
	if err := ValidateConfigurationSurfaceBindings(valid); err != nil {
		t.Fatalf("valid published bindings rejected: %v", err)
	}

	tests := []struct {
		name    string
		binding ConfigurationSurfaceBinding
	}{
		{"missing view", func() ConfigurationSurfaceBinding { b := valid[0]; b.ViewName = ""; return b }()},
		{"missing revision", func() ConfigurationSurfaceBinding { b := valid[0]; b.CatalogRevision = 0; return b }()},
		{"missing scope", func() ConfigurationSurfaceBinding { b := valid[0]; b.BusinessScope = BusinessScope{}; return b }()},
		{"invalid owner", func() ConfigurationSurfaceBinding { b := valid[0]; b.OwnerModuleID = "../../private"; return b }()},
		{"missing schema", func() ConfigurationSurfaceBinding { b := valid[0]; b.OutputSchema = SchemaRef{}; return b }()},
		{"noncanonical operation", func() ConfigurationSurfaceBinding {
			b := valid[2]
			b.OperationRef = "sales.orders~sales.invoice~Issue"
			return b
		}()},
		{"query without input", func() ConfigurationSurfaceBinding { b := valid[0]; b.InputSchema = nil; return b }()},
		{"projection with input", func() ConfigurationSurfaceBinding {
			b := valid[1]
			input := SchemaRef{ID: "extra", Version: 1}
			b.InputSchema = &input
			return b
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.binding.Validate(); err == nil {
				t.Fatal("invalid binding was accepted")
			}
		})
	}
	if err := ValidateConfigurationSurfaceBindings([]ConfigurationSurfaceBinding{valid[0], valid[0]}); err == nil {
		t.Fatal("duplicate binding was accepted")
	}
	secondView := valid[0]
	secondView.ViewName = "Invoice Detail"
	if err := ValidateConfigurationSurfaceBindings([]ConfigurationSurfaceBinding{valid[0], secondView}); err != nil {
		t.Fatalf("one published query could not be bound to two distinct views: %v", err)
	}
}
