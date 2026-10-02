//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/schema"
	ksite "github.com/asenawritescode/kora/site"
)

func TestConfiguredCommandCommitsAndRollsBackAcrossDialects(t *testing.T) {
	for _, tc := range []struct {
		driver string
		env    string
	}{
		{driver: "postgres", env: "KORA_API_LIVE_POSTGRES_DSN"},
		{driver: "libsql", env: "KORA_API_LIVE_LIBSQL_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s database", tc.env, tc.driver)
			}
			database, dialect := openKernelDialectDatabase(t, tc.driver, dsn)
			if err := ksite.BootstrapSystemTables(database, dialect); err != nil {
				t.Fatalf("bootstrap %s tables: %v", tc.driver, err)
			}

			suffix := fmt.Sprintf("%x", time.Now().UnixNano())
			siteName := "defined-command-" + tc.driver + "-" + suffix
			doctypeName := "CommandTask" + suffix
			dt := &doctype.DocType{Name: doctypeName, Fields: []doctype.Field{
				{Fieldname: "title", Fieldtype: "Data", Reqd: true},
				{Fieldname: "serial", Fieldtype: "Data", Reqd: true, Unique: true},
				{Fieldname: "status", Fieldtype: "Data"},
			}}
			registry := doctype.NewRegistry()
			registry.LoadFull([]*doctype.DocType{dt}, []*doctype.Role{{Name: doctype.AdminRole}}, []*doctype.Permission{{
				Doctype: doctypeName, Role: doctype.AdminRole, Read: true, Create: true, Write: true, Delete: true,
			}})
			cleanupKernelDialectRecords(t, database, dialect, siteName, doctypeName)
			if err := schema.MigrateSiteFromRegistry(database, siteName, registry, dialect); err != nil {
				t.Fatalf("migrate %s DocType: %v", tc.driver, err)
			}

			definitionYAML := fmt.Sprintf(`
name: register
namespace: migration_test
version: 1
input:
  record: %s
transaction:
  - create:
      record: %s
      values:
        title: $input.title
        serial: $input.serial
  - update:
      record: %s
      name: "$input.supervisor_ref"
      values:
        status: Done
emit:
  - command.registered
`, doctypeName, doctypeName, doctypeName)
			definition, err := kernel.ParseCommandResource([]byte(definitionYAML))
			if err != nil {
				t.Fatalf("parse configured command: %v", err)
			}

			writer := outbox.NewSQLWriter(dialect)
			k := kernel.New(dialect, writer)
			k.Commands = kernel.NewCommandRegistry()
			if err := k.Commands.Register(definition); err != nil {
				t.Fatalf("register configured command: %v", err)
			}
			baseContext := kernel.OperationContext{
				Site: siteName, User: "migration@example.test", Roles: []string{doctype.AdminRole},
				Actor:  contract.ActorContext{PrincipalID: "migration@example.test", PrincipalType: contract.PrincipalHuman, Site: siteName, AuthenticatedAt: time.Now()},
				Source: kernel.SourceIntegration,
			}

			seedData, _ := json.Marshal(map[string]any{"title": "Supervisor", "serial": "SUP-1"})
			seedPayload, _ := json.Marshal(kernel.RecordCreatePayload{Doctype: doctypeName, Data: seedData})
			seedResult, seedErr := k.Execute(context.Background(), database, registry, kernel.Operation{Context: baseContext, Command: kernel.CommandRecordCreate, Payload: seedPayload})
			if seedErr != nil || seedResult.Status != contract.StatusCompleted {
				t.Fatalf("seed supervisor: result=%+v error=%+v", seedResult, seedErr)
			}
			var supervisor kernel.ResultData
			if err := json.Unmarshal(seedResult.Data, &supervisor); err != nil {
				t.Fatalf("decode supervisor result: %v", err)
			}

			baselineRows := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(dt.RawTableName()))
			baselineAudit := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ?", siteName)
			baselineOutbox := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", siteName)
			failedKey := "defined-failure-" + suffix
			failureInput, _ := json.Marshal(map[string]any{"data": map[string]any{
				"title": "Must roll back", "serial": "ROLLBACK-1", "supervisor_ref": "MISSING-" + suffix,
			}})
			failedContext := baseContext
			failedContext.IdempotencyKey = failedKey
			if _, commandErr := k.Execute(context.Background(), database, registry, kernel.Operation{
				Context: failedContext, Command: definition.FullName(), Payload: failureInput,
			}); commandErr == nil || commandErr.Type != contract.CodeNotFound {
				t.Fatalf("missing update target error=%v, want NOT_FOUND", commandErr)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(dt.RawTableName())); got != baselineRows {
				t.Fatalf("failed command left %d record rows; baseline=%d", got, baselineRows)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ?", siteName); got != baselineAudit {
				t.Fatalf("failed command changed audit count: got=%d baseline=%d", got, baselineAudit)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", siteName); got != baselineOutbox {
				t.Fatalf("failed command changed outbox count: got=%d baseline=%d", got, baselineOutbox)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND idempotency_key = ?", siteName, failedKey); got != 0 {
				t.Fatalf("failed command left %d idempotency receipts", got)
			}

			successKey := "defined-success-" + suffix
			successInput, _ := json.Marshal(map[string]any{"data": map[string]any{
				"title": "Daisy", "serial": "ANM-1", "supervisor_ref": supervisor.Name,
			}})
			successContext := baseContext
			successContext.IdempotencyKey = successKey
			successResult, successErr := k.Execute(context.Background(), database, registry, kernel.Operation{
				Context: successContext, Command: definition.FullName(), Payload: successInput,
			})
			if successErr != nil || successResult.Status != contract.StatusCompleted {
				t.Fatalf("configured command: result=%+v error=%+v", successResult, successErr)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(dt.RawTableName())+" WHERE serial = ?", "ANM-1"); got != 1 {
				t.Fatalf("configured create rows=%d, want 1", got)
			}
			var status string
			if err := database.QueryRow(kdb.Rebind(dialect, "SELECT status FROM "+dialect.QuoteIdent(dt.RawTableName())+" WHERE name = ?"), supervisor.Name).Scan(&status); err != nil {
				t.Fatalf("read updated supervisor: %v", err)
			}
			if status != "Done" {
				t.Fatalf("supervisor status=%q, want Done", status)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND command_name = ? AND status = 'completed'", siteName, definition.FullName()); got != 1 {
				t.Fatalf("configured command audit rows=%d, want 1", got)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND idempotency_key = ?", siteName, successKey); got != 1 {
				t.Fatalf("configured command receipts=%d, want 1", got)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", siteName); got <= baselineOutbox {
				t.Fatalf("successful configured command added no outbox events: before=%d after=%d", baselineOutbox, got)
			}
		})
	}
}

func TestPublicFormProjectionAndReplayAcrossDialects(t *testing.T) {
	for _, tc := range []struct {
		driver string
		env    string
	}{
		{driver: "postgres", env: "KORA_API_LIVE_POSTGRES_DSN"},
		{driver: "libsql", env: "KORA_API_LIVE_LIBSQL_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s database", tc.env, tc.driver)
			}
			database, dialect := openKernelDialectDatabase(t, tc.driver, dsn)
			if err := ksite.BootstrapSystemTables(database, dialect); err != nil {
				t.Fatalf("bootstrap %s tables: %v", tc.driver, err)
			}

			suffix := fmt.Sprintf("%x", time.Now().UnixNano())
			siteName := "public-form-" + tc.driver + "-" + suffix
			doctypeName := "PublicRequest" + suffix
			dt := &doctype.DocType{Name: doctypeName, PublicAccess: &doctype.PublicAccess{Enabled: true, Fields: []string{"title"}}, Fields: []doctype.Field{
				{Fieldname: "title", Fieldtype: "Data", Reqd: true},
				{Fieldname: "serial", Fieldtype: "Data"},
				{Fieldname: "status", Fieldtype: "Data"},
			}}
			registry := doctype.NewRegistry()
			registry.LoadFull([]*doctype.DocType{dt}, []*doctype.Role{{Name: doctype.AdminRole}}, []*doctype.Permission{{
				Doctype: doctypeName, Role: doctype.AdminRole, Read: true, Create: true, Write: true, Delete: true,
			}})
			route := "/public-request-" + suffix
			registry.Views.Register(&doctype.View{
				Name: "Public request", Route: route, SourceDocType: doctypeName,
				PublicAccess: &doctype.ViewPublicAccess{Enabled: true, AllowMutations: true},
			})
			cleanupKernelDialectRecords(t, database, dialect, siteName, doctypeName)
			if err := schema.MigrateSiteFromRegistry(database, siteName, registry, dialect); err != nil {
				t.Fatalf("migrate %s DocType: %v", tc.driver, err)
			}

			k := kernel.New(dialect, outbox.NewSQLWriter(dialect))
			payload, _ := json.Marshal(map[string]any{
				"doctype": doctypeName, "public_route": route,
				"data": map[string]any{"title": "Contact us", "serial": "must-not-persist", "status": "private"},
			})
			operationContext := kernel.OperationContext{
				Site: siteName, User: "public", Source: kernel.SourceHTTP, IdempotencyKey: "public-submit-" + suffix,
				Actor: contract.ActorContext{PrincipalID: "public-form", PrincipalType: contract.PrincipalPublic, Site: siteName, AuthenticatedAt: time.Now()},
			}
			operation := kernel.Operation{Context: operationContext, Command: kernel.CommandPublicFormSubmit, Payload: payload}
			created, createErr := k.Execute(context.Background(), database, registry, operation)
			if createErr != nil || created.Status != contract.StatusCompleted {
				t.Fatalf("public form submit: result=%+v error=%+v", created, createErr)
			}
			var record kernel.ResultData
			if err := json.Unmarshal(created.Data, &record); err != nil {
				t.Fatalf("decode public result: %v", err)
			}
			if record.Name == "" || record.Document["title"] != "Contact us" || record.Document["serial"] != nil || record.Document["status"] != nil {
				t.Fatalf("public result has wrong field projection: %+v", record.Document)
			}
			var title, serial, status any
			query := "SELECT title, serial, status FROM " + dialect.QuoteIdent(dt.RawTableName()) + " WHERE name = ?"
			if err := database.QueryRow(kdb.Rebind(dialect, query), record.Name).Scan(&title, &serial, &status); err != nil {
				t.Fatalf("read public record: %v", err)
			}
			if fmt.Sprint(title) != "Contact us" || serial != nil || status != nil {
				t.Fatalf("unlisted public values persisted: title=%v serial=%v status=%v", title, serial, status)
			}

			beforeAudit := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ?", siteName)
			beforeOutbox := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", siteName)
			replay, replayErr := k.Execute(context.Background(), database, registry, operation)
			if replayErr != nil || replay.Status != contract.StatusCompleted || !replay.Replayed || string(replay.Data) != string(created.Data) {
				t.Fatalf("public form replay: result=%+v error=%+v", replay, replayErr)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(dt.RawTableName())); got != 1 {
				t.Fatalf("public replay created %d rows, want 1", got)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ?", siteName); got != beforeAudit {
				t.Fatalf("public replay changed audit count: before=%d after=%d", beforeAudit, got)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", siteName); got != beforeOutbox {
				t.Fatalf("public replay changed outbox count: before=%d after=%d", beforeOutbox, got)
			}

			deniedPayload, _ := json.Marshal(map[string]any{
				"doctype": doctypeName, "public_route": "/disabled", "data": map[string]any{"title": "must-deny"},
			})
			deniedContext := operationContext
			deniedContext.IdempotencyKey = "disabled-submit-" + suffix
			denied, deniedErr := k.Execute(context.Background(), database, registry, kernel.Operation{
				Context: deniedContext, Command: kernel.CommandPublicFormSubmit, Payload: deniedPayload,
			})
			if deniedErr == nil || deniedErr.Type != contract.CodePermissionDenied || denied.Status != contract.StatusRejected {
				t.Fatalf("disabled public route: result=%+v error=%+v", denied, deniedErr)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(dt.RawTableName())); got != 1 {
				t.Fatalf("disabled route created %d rows, want 1", got)
			}
		})
	}
}
