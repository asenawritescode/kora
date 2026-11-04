package kernel

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
)

func TestModuleLifecycleAdmissionFencesOwnerCommands(t *testing.T) {
	for _, state := range []string{"active", "draining", "disabled"} {
		t.Run(state, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			mock.ExpectBegin()
			tx, err := database.Begin()
			if err != nil {
				t.Fatal(err)
			}
			registry := doctype.NewRegistry()
			registry.Register(&doctype.DocType{Name: "Invoice", ModuleOwnerID: "finance.ar"})
			payload, _ := json.Marshal(RecordCreatePayload{Doctype: "Invoice", Data: json.RawMessage(`{}`)})
			snapshot, _ := json.Marshal(contract.ModuleSnapshot{TenantID: "tenant-a", Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: "finance.ar", State: state}}})
			mock.ExpectExec("UPDATE _kora_module_snapshot SET revision = revision WHERE tenant_id = \\?").WithArgs("tenant-a").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery("SELECT snapshot_json FROM _kora_module_snapshot WHERE tenant_id = \\?").WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"snapshot_json"}).AddRow(string(snapshot)))
			op := Operation{Command: CommandRecordCreate, Payload: payload, Context: OperationContext{TenantID: "tenant-a", ModuleExecution: &contract.ModuleExecution{}}}
			got := enforceModuleLifecycleAdmissionTx(context.Background(), tx, kdb.Resolve("libsql"), op, registry, nil)
			if state == "active" && got != nil {
				t.Fatalf("active module rejected: %v", got)
			}
			if state != "active" && (got == nil || got.Type != contract.CodePermissionDenied) {
				t.Fatalf("%s module admission error = %v", state, got)
			}
			_ = tx.Rollback()
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestModuleLifecycleAdmissionFencesGenericWritesWithoutModuleIdentity(t *testing.T) {
	for _, test := range []struct {
		state  string
		denied bool
	}{{"active", false}, {"draining", true}, {"disabled", true}, {"removed", true}} {
		t.Run(test.state, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			mock.ExpectBegin()
			tx, err := database.Begin()
			if err != nil {
				t.Fatal(err)
			}
			registry := doctype.NewRegistry()
			registry.Register(&doctype.DocType{Name: "Invoice", ModuleOwnerID: "finance.ar"})
			payload, _ := json.Marshal(RecordCreatePayload{Doctype: "Invoice", Data: json.RawMessage(`{}`)})
			snapshot, _ := json.Marshal(contract.ModuleSnapshot{TenantID: "tenant-a", Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: "finance.ar", State: test.state}}})
			mock.ExpectExec("UPDATE _kora_module_snapshot SET revision = revision WHERE tenant_id = \\?").WithArgs("tenant-a").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery("SELECT snapshot_json FROM _kora_module_snapshot WHERE tenant_id = \\?").WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"snapshot_json"}).AddRow(string(snapshot)))
			op := Operation{Command: CommandRecordCreate, Payload: payload, Context: OperationContext{TenantID: "tenant-a"}}
			got := enforceModuleLifecycleAdmissionTx(context.Background(), tx, kdb.Resolve("libsql"), op, registry, nil)
			if test.denied && (got == nil || got.Type != contract.CodePermissionDenied) {
				t.Fatalf("generic %s write admission = %v, want permission denied", test.state, got)
			}
			if !test.denied && got != nil {
				t.Fatalf("generic active-module write rejected: %v", got)
			}
			_ = tx.Rollback()
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDrainingModuleAllowsOnlyPreDrainPinnedWorkflowCommand(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectBegin()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	changedAt := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	snapshot, _ := json.Marshal(contract.ModuleSnapshot{TenantID: "tenant-a", Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: "finance.ar", State: string(contract.ModuleDraining), StateChangedAt: changedAt}}})
	mock.ExpectExec("UPDATE _kora_module_snapshot SET revision = revision WHERE tenant_id = \\?").WithArgs("tenant-a").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT snapshot_json FROM _kora_module_snapshot WHERE tenant_id = \\?").WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"snapshot_json"}).AddRow(string(snapshot)))
	mock.ExpectQuery("SELECT MIN\\(started_at\\) FROM _kora_workflow_attempt").WithArgs("tenant-a", "instance-1", "issue", "workflow-key-1").WillReturnRows(sqlmock.NewRows([]string{"started_at"}).AddRow(changedAt.Add(-time.Minute).Format(time.RFC3339Nano)))
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Invoice", ModuleOwnerID: "finance.ar"})
	module, err := contract.NewCommandModuleExecution(contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, "sales.orders", "finance.ar", 1, "finance.invoice.issue", "finance.invoice.issue")
	if err != nil {
		t.Fatal(err)
	}
	module, err = module.WithWorkflowCommandPin("instance-1", "issue", "workflow-key-1")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(RecordCreatePayload{Doctype: "Invoice", Data: json.RawMessage(`{}`)})
	op := Operation{Command: CommandRecordCreate, Payload: payload, Context: OperationContext{TenantID: "tenant-a", ModuleExecution: &module}}
	if got := enforceModuleLifecycleAdmissionTx(context.Background(), tx, kdb.Resolve("libsql"), op, registry, nil); got != nil {
		t.Fatalf("pre-drain pinned command rejected: %v", got)
	}
	_ = tx.Rollback()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGenericMutationCannotUseCommandOnlyModuleExecution(t *testing.T) {
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Invoice", ModuleOwnerID: "finance.ar", ModuleOwnershipMode: string(contract.MutationCommands)})
	payload, _ := json.Marshal(RecordCreatePayload{Doctype: "Invoice", Data: json.RawMessage(`{}`)})
	op := Operation{Command: CommandRecordCreate, Payload: payload, Context: OperationContext{TenantID: "tenant-a"}}
	if !mutationTouchesCommandOnly(registry, op) {
		t.Fatal("protected record was not recognized")
	}
	if err := authorizeOp(registry, op, CommandDefinition{Name: CommandRecordCreate}, nil); err == nil {
		t.Fatal("generic mutation authorized without trusted module execution")
	}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	execution, err := contract.NewCommandModuleExecution(scope, "sales.orders", "finance.ar", 3, "finance.issue_invoice", "finance.issue_invoice")
	if err != nil {
		t.Fatal(err)
	}
	op.Context.ModuleExecution = &execution
	if err := authorizeOp(registry, op, CommandDefinition{Name: CommandRecordCreate}, nil); err == nil {
		t.Fatal("domain command execution authorized generic record command")
	}
	// User-supplied struct values cannot set the private verification seal.
	forged := contract.ModuleExecution{TenantID: "tenant-a", CallerModuleID: "sales.orders", OwnerModuleID: "finance.ar", BindingRevision: 3, AllowedOperation: CommandRecordCreate, Scope: scope}
	op.Context.ModuleExecution = &forged
	if validModuleExecution(op.Context.ModuleExecution, op, registry) {
		t.Fatal("forged module execution was accepted")
	}
}

func TestVerifiedOwnerCommandDoesNotRequirePrivateCRUDGrant(t *testing.T) {
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{
		Name:                "Invoice",
		ModuleOwnerID:       "finance.ar",
		ModuleOwnershipMode: string(contract.MutationCommands),
	})
	command := &CommandResource{
		Name:          "issue",
		Namespace:     "finance.invoice",
		Version:       1,
		ModuleOwnerID: "finance.ar",
		Capability:    "billing.invoice",
		Operation:     "issue",
		InputSchema:   contract.SchemaRef{ID: "billing.invoice.issue.input", Version: 1},
		OutputSchema:  contract.SchemaRef{ID: "billing.invoice.issue.output", Version: 1},
		Steps: []CommandStep{{Create: &StepCreate{
			Record: "Invoice",
			Values: map[string]string{"title": "$input.title"},
		}}},
	}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{
		PrincipalID: "extension:scoped", PrincipalType: contract.PrincipalService, Site: "tenant-a",
		AuthenticatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal("verified actor:", err)
	}
	moduleExecution, err := contract.NewCommandModuleExecution(scope, "sales.orders", "finance.ar", 1, command.FullName(), command.FullName())
	if err != nil {
		t.Fatal("module execution:", err)
	}
	op := Operation{
		Command: command.FullName(),
		Payload: json.RawMessage(`{"title":"published invoice"}`),
		Context: OperationContext{TenantID: "tenant-a", Site: "tenant-a", BusinessScope: scope, Execution: execution, ModuleExecution: &moduleExecution},
	}
	if authErr := authorizeOp(registry, op, CommandDefinition{Name: command.FullName()}, command); authErr != nil {
		t.Fatalf("verified owner command required private CRUD grant: %v", authErr)
	}
}

func TestCommandOnlyResourcesRejectGenericMutationSources(t *testing.T) {
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Invoice", ModuleOwnerID: "finance.ar", ModuleOwnershipMode: string(contract.MutationCommands)})

	tests := []struct {
		name    string
		command string
		payload string
		source  Source
	}{
		{name: "http create", command: CommandRecordCreate, payload: `{"doctype":"Invoice","data":{}}`, source: SourceHTTP},
		{name: "ai update", command: CommandRecordUpdate, payload: `{"doctype":"Invoice","name":"INV-1","data":{}}`, source: SourceAI},
		{name: "mcp delete", command: CommandRecordDelete, payload: `{"doctype":"Invoice","name":"INV-1"}`, source: SourceMCP},
		{name: "scheduled worker generic create", command: CommandRecordCreate, payload: `{"doctype":"Invoice","data":{}}`, source: SourceWorker},
		{name: "workflow transition", command: CommandWorkflowStatusTransition, payload: `{"doctype":"Invoice","name":"INV-1","action":"approve"}`, source: SourceWorker},
		{name: "public form", command: CommandPublicFormSubmit, payload: `{"doctype":"Invoice","public_route":"apply","data":{}}`, source: SourceHTTP},
		{name: "bundle with protected child", command: CommandRecordMutateBundle, payload: `{"doctype":"Task","records":[{"key":"task","doctype":"Task","data":{}},{"key":"invoice","doctype":"Invoice","data":{}}]}`, source: SourceAI},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			op := Operation{
				Command: test.command,
				Payload: json.RawMessage(test.payload),
				Context: OperationContext{TenantID: "tenant-a", Source: test.source, Roles: []string{doctype.AdminRole}, UserRole: doctype.AdminRole},
			}
			if err := authorizeOp(registry, op, CommandDefinition{Name: test.command}, nil); err == nil || err.Type != contract.CodePermissionDenied {
				t.Fatalf("broad actor role authorized generic command against commands-only resource: got %v", err)
			}
		})
	}
}

func TestDynamicCommandCannotBypassCommandOnlyOwnership(t *testing.T) {
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Invoice", ModuleOwnerID: "finance.ar", ModuleOwnershipMode: string(contract.MutationCommands)})
	command := &CommandResource{Name: "issue", Namespace: "finance", Version: 1, Input: CommandInput{Record: "Invoice"}, ModuleOwnerID: "finance.ar", Capability: "billing.issue", Operation: "issue", InputSchema: contract.SchemaRef{ID: "invoice.issue.input", Version: 1}, OutputSchema: contract.SchemaRef{ID: "invoice.issue.output", Version: 1}, Steps: []CommandStep{{Create: &StepCreate{Record: "Invoice"}}}}
	op := Operation{Command: command.FullName(), Context: OperationContext{TenantID: "tenant-a", BusinessScope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}}}
	if !dynamicCommandTouchesCommandOnly(registry, command) {
		t.Fatal("dynamic command's protected target was not detected")
	}
	if validDynamicModuleExecution(nil, op, registry, command) {
		t.Fatal("missing module execution accepted")
	}
	forged := contract.ModuleExecution{TenantID: "tenant-a", CallerModuleID: "sales.orders", OwnerModuleID: "finance.ar", BindingRevision: 3, AllowedOperation: op.Command, AuthorizedCommand: op.Command, Scope: op.Context.BusinessScope}
	op.Context.ModuleExecution = &forged
	if validDynamicModuleExecution(&forged, op, registry, command) {
		t.Fatal("forged module execution accepted")
	}
	wrongOwner, err := contract.NewCommandModuleExecution(op.Context.BusinessScope, "sales.orders", "sales.orders", 3, op.Command, op.Command)
	if err != nil {
		t.Fatal(err)
	}
	if validDynamicModuleExecution(&wrongOwner, op, registry, command) {
		t.Fatal("non-owner command execution accepted")
	}
	valid, err := contract.NewCommandModuleExecution(op.Context.BusinessScope, "sales.orders", "finance.ar", 3, op.Command, op.Command)
	if err != nil {
		t.Fatal(err)
	}
	if !validDynamicModuleExecution(&valid, op, registry, command) {
		t.Fatal("valid owner command execution rejected")
	}
}
