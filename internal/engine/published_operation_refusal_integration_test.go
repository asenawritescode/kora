//go:build integration

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/plugin"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/schema"
	"github.com/asenawritescode/kora/internal/site"
)

// acceptAllSchemas registers every schema ref and accepts any JSON, so the
// refusals under test come from the command's own validation and guards.
type acceptAllSchemas map[contract.SchemaRef]bool

func (s acceptAllSchemas) Registered(ref contract.SchemaRef) bool { return s[ref] }
func (s acceptAllSchemas) Validate(_ contract.SchemaRef, raw json.RawMessage) error {
	if !json.Valid(raw) {
		return errors.New("invalid JSON")
	}
	return nil
}

// A refused published operation must reply with the same field-level details
// the record write path gives: the receipt keeps them, and so does a replay.
func TestPublishedOperationRefusalKeepsDetailsAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_ENGINE_PUBOP_REFUSAL_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_ENGINE_PUBOP_REFUSAL_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable database", tc.env)
			}
			runPublishedRefusalFixture(t, tc.driver, dsn)
		})
	}
}

func runPublishedRefusalFixture(t *testing.T, driver, dsn string) {
	ctx := context.Background()
	database, dbName := isolatedDB(t, driver, dsn)
	dialect := kdb.Resolve(driver)
	if err := site.BootstrapSystemTables(ctx, database, dialect); err != nil {
		t.Fatal("bootstrap system tables:", err)
	}
	kernelTables, outboxTables := kdb.KernelTablesPostgres(), kdb.OutboxTablesPostgres()
	if driver == "mysql" {
		kernelTables, outboxTables = kdb.KernelTablesMySQL(), kdb.OutboxTablesMySQL()
	}
	for _, statements := range [][]string{kernelTables, outboxTables} {
		for _, statement := range statements {
			if _, err := database.ExecContext(ctx, statement); err != nil {
				t.Fatalf("create kernel support schema: %v\n%s", err, statement)
			}
		}
	}
	tenant := contract.TenantID("tenant-" + dbName)
	invoice := &doctype.DocType{Name: "RefusalInvoice", ModuleOwnerID: "finance.ar", ModuleOwnershipMode: string(contract.MutationCommands), BusinessScopePolicy: contract.ScopeEntityOwned, BusinessScopeField: "legal_entity_id", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}, {Fieldname: "legal_entity_id", Fieldtype: "Data", Reqd: true}}}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{invoice}, []*doctype.Role{{Name: doctype.AdminRole}}, nil)
	if err := schema.MigrateSiteFromRegistry(database, dbName, registry, dialect); err != nil {
		t.Fatal("migrate owner resource:", err)
	}
	issue, err := kernel.ParseCommandResource([]byte("name: issue\nnamespace: finance.invoice\nversion: 1\ninput:\n  record: " + invoice.Name + "\nmodule_owner_id: finance.ar\ncapability: billing.invoice\noperation: issue\ninput_schema:\n  id: billing.invoice.issue.input\n  version: 1\noutput_schema:\n  id: billing.invoice.issue.output\n  version: 1\ntransaction:\n  - create:\n      record: " + invoice.Name + "\n      values:\n        title: $input.title\n"))
	if err != nil {
		t.Fatal("parse issue command:", err)
	}
	refund, err := kernel.ParseCommandResource([]byte("name: refund\nnamespace: finance.invoice\nversion: 1\ninput:\n  record: " + invoice.Name + "\nmodule_owner_id: finance.ar\ncapability: billing.invoice\noperation: refund\ninput_schema:\n  id: billing.invoice.refund.input\n  version: 1\noutput_schema:\n  id: billing.invoice.refund.output\n  version: 1\nrequire:\n  - equal: [\"1\", \"2\"]\n    message: Only a paid sale can be returned\ntransaction:\n  - create:\n      record: " + invoice.Name + "\n      values:\n        title: $input.title\n"))
	if err != nil {
		t.Fatal("parse refund command:", err)
	}
	commands := kernel.NewCommandRegistry()
	for _, command := range []*kernel.CommandResource{issue, refund} {
		if err = commands.Register(command); err != nil {
			t.Fatal("register owner command:", err)
		}
	}
	commandKernel := kernel.New(dialect, outbox.NewSQLWriter(dialect))
	commandKernel.Commands = commands
	schemas := acceptAllSchemas{}
	for _, command := range []*kernel.CommandResource{issue, refund} {
		schemas[command.InputSchema] = true
		schemas[command.OutputSchema] = true
	}
	commandKernel.CommandSchemas = schemas
	offer := contract.CapabilityOffer{Name: "billing.invoice", Contract: issue.OutputSchema, Operations: []contract.OperationSpec{
		{Name: "issue", Input: issue.InputSchema, Output: issue.OutputSchema, Effect: contract.EffectLocal},
		{Name: "refund", Input: refund.InputSchema, Output: refund.OutputSchema, Effect: contract.EffectLocal},
	}}
	published := func(op string) contract.PublishedOperation {
		return contract.PublishedOperation{Name: op, Capability: offer.Name, Operation: op, ScopePolicy: contract.ScopeEntityOwned, AllowedCallers: []contract.ModuleID{"sales.orders"}}
	}
	owner := contract.ModuleDescriptor{ID: "finance.ar", Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}, PublishedOperations: []contract.PublishedOperation{published("issue"), published("refund")}}
	caller := contract.ModuleDescriptor{ID: "sales.orders", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: offer.Name, Contract: offer.Contract, Required: true}}}
	providers, err := registeredKernelCommandProviders(moduleProviderSource{tenant: tenant, db: database, registry: registry, modules: []contract.ModuleDescriptor{caller, owner}, schemas: schemas}, commands, commandKernel)
	if err != nil {
		t.Fatal("register owner command provider:", err)
	}
	scope := contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	for _, op := range []string{"issue", "refund"} {
		registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.finance.ar.operation." + op, Role: doctype.AdminRole, Write: true, Scope: scope})
	}
	modules := []contract.ModuleDescriptor{caller, owner}
	moduleStore := configstore.NewModuleStore(database, dialect)
	if err = moduleStore.EnsureSchema(); err != nil {
		t.Fatal("ensure module store:", err)
	}
	resolve := func(requested contract.BusinessScope) (plugin.BindingPlan, error) {
		plan, resolveErr := plugin.ResolveModules(requested, modules, providers, nil, 4)
		plan.LeaseAdmission = moduleStore
		return plan, resolveErr
	}
	snapshot := contract.ModuleSnapshot{TenantID: tenant, Modules: modules, Installations: []contract.ModuleInstallation{
		{TenantID: tenant, ModuleID: caller.ID, Version: caller.Version, ArtifactDigest: strings.Repeat("a", 64), State: string(contract.ModuleActive), Revision: 1},
		{TenantID: tenant, ModuleID: owner.ID, Version: owner.Version, ArtifactDigest: strings.Repeat("b", 64), State: string(contract.ModuleActive), Revision: 1},
	}}
	activation, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin module activation:", err)
	}
	if _, err = moduleStore.CommitActivation(ctx, activation, 0, snapshot); err != nil {
		_ = activation.Rollback()
		t.Fatal("persist module ownership:", err)
	}
	if err = activation.Commit(); err != nil {
		t.Fatal("commit module ownership:", err)
	}
	execution, err := policy.NewVerifiedExecutionWithGrants(tenant, contract.ActorContext{PrincipalID: "finance-operator", PrincipalType: contract.PrincipalHuman, Site: string(tenant), Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now().UTC()}, []policy.Grant{{Resource: invoice.Name, Operation: "create", Scope: scope}})
	if err != nil {
		t.Fatal("create verified actor execution:", err)
	}
	runtime := publishedOperationRuntime{store: moduleStore, resolve: resolve, commandKernel: commandKernel, commandRegistry: commands, registry: registry}
	run := func(op contract.PublishedOperation, inputSchema contract.SchemaRef, key string, input string) (contract.OperationReceipt, error) {
		meta := contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(key)}
		target := publishedOperationTarget{execution: execution, scope: scope, owner: owner.ID, published: op}
		return executePublishedOperation(ctx, runtime, target, publishedOperationInvocation{inputSchema: inputSchema, meta: meta, input: []byte(input)})
	}

	// A missing required field: the receipt names the field, and a replay of the key says the same.
	missing, err := run(owner.PublishedOperations[0], issue.InputSchema, "refusal-missing-title", `{}`)
	if err != nil || missing.Status != contract.ReceiptRejected || missing.Error == nil {
		t.Fatalf("missing required field = (%+v, %v), want a rejected receipt", missing, err)
	}
	if missing.Error.Type != contract.CodeValidationFailed || len(missing.Error.Details) == 0 {
		t.Fatalf("missing required field error = %+v, want validation details", missing.Error)
	}
	assertRefusedField(t, missing.Error.Details, "title")
	missingReplay, err := run(owner.PublishedOperations[0], issue.InputSchema, "refusal-missing-title", `{}`)
	if err != nil || missingReplay.ExecutionID != missing.ExecutionID || string(missingReplay.Error.Details) != string(missing.Error.Details) {
		t.Fatalf("replay of refused key = (%+v, %v), want the same receipt %+v", missingReplay, err, missing)
	}

	// A command guard refusal: the message the owner wrote is kept on the receipt.
	guarded, err := run(owner.PublishedOperations[1], refund.InputSchema, "refusal-guard", `{"title":"guarded"}`)
	if err != nil || guarded.Status != contract.ReceiptRejected || guarded.Error == nil || guarded.Error.Message != "Only a paid sale can be returned" {
		t.Fatalf("guard refusal = (%+v, %v), want rejected with the guard message", guarded, err)
	}
	guardedReplay, err := run(owner.PublishedOperations[1], refund.InputSchema, "refusal-guard", `{"title":"guarded"}`)
	if err != nil || guardedReplay.ExecutionID != guarded.ExecutionID || guardedReplay.Error.Message != guarded.Error.Message {
		t.Fatalf("replay of guard refusal = (%+v, %v), want the same receipt", guardedReplay, err)
	}
}

func assertRefusedField(t *testing.T, raw json.RawMessage, field string) {
	t.Helper()
	var fields []struct {
		DocType string `json:"doctype"`
		Field   string `json:"field"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("details %s are not a field list: %v", raw, err)
	}
	for _, f := range fields {
		if f.Field == field && f.Message != "" && f.DocType == "RefusalInvoice" {
			return
		}
	}
	t.Fatalf("details %s lack a %q field refusal with a message", raw, field)
}
