package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	knet "github.com/asenawritescode/kora/internal/net"
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

func TestConfigureTenantWorkflowRequiresItsDependencies(t *testing.T) {
	loaded := &knet.LoadedSite{DB: new(sql.DB)}
	admission, err := dbaccess.NewAdmission(1, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	complete := tenantWorkflowDeps{dialect: &db.MySQLDialect{}, admission: admission, domainKernel: kernel.New(&db.MySQLDialect{}, nil)}
	tests := []struct {
		name   string
		loaded *knet.LoadedSite
		deps   func(tenantWorkflowDeps) tenantWorkflowDeps
	}{
		{"no runtime", nil, func(d tenantWorkflowDeps) tenantWorkflowDeps { return d }},
		{"no dialect", loaded, func(d tenantWorkflowDeps) tenantWorkflowDeps { d.dialect = nil; return d }},
		{"no admission", loaded, func(d tenantWorkflowDeps) tenantWorkflowDeps { d.admission = nil; return d }},
		{"no domain kernel", loaded, func(d tenantWorkflowDeps) tenantWorkflowDeps { d.domainKernel = nil; return d }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker, definitions, instances, err := configureTenantWorkflow(context.Background(), nil, tt.loaded, tt.deps(complete))
			if err == nil || worker != nil || definitions != nil || instances != nil {
				t.Fatalf("configureTenantWorkflow = (%v, %v, %v, %v), want an error and no services", worker, definitions, instances, err)
			}
		})
	}
}
