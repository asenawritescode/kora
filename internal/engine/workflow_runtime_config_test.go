package engine

import (
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
)

func TestSelfHostedWorkflowConfigIsDomainNeutral(t *testing.T) {
	cfg := SelfHostedWorkflowConfig()
	if cfg == nil || cfg.Schemas == nil || cfg.Waits == nil || cfg.Evaluator == nil {
		t.Fatal("self-hosted workflow config is incomplete")
	}
	if len(cfg.Registrations) != 0 || len(cfg.LocalBuilders) != 0 {
		t.Fatalf("self-hosted baseline unexpectedly registers actions: registrations=%d builders=%d", len(cfg.Registrations), len(cfg.LocalBuilders))
	}
	if len(cfg.ServiceGrants) != 0 {
		t.Fatalf("domain-neutral workflow baseline must not contain business grants: %+v", cfg.ServiceGrants)
	}
	if cfg.Schemas.Registered(contract.SchemaRef{ID: "unregistered", Version: 1}) {
		t.Fatal("self-hosted workflow accepted an unregistered payload schema")
	}
	if cfg.Waits.RegisteredCorrelationField("invoice.paid", "invoice_id") {
		t.Fatal("self-hosted workflow accepted an unregistered event correlation field")
	}
}

func TestTenantWorkflowSchemasRecognizesEmailOption(t *testing.T) {
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Contact", Fields: []doctype.Field{{Fieldname: "email", Fieldtype: "Data", Options: "Email"}}})
	schemas := tenantWorkflowSchemas{registry: registry}
	if fieldType, ok := schemas.RecordFieldType("Contact", "email"); !ok || fieldType != "Email" {
		t.Fatalf("email field type = %q, found=%t", fieldType, ok)
	}
}
