//go:build integration

package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/schema"
	ksite "github.com/asenawritescode/kora/site"
	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestLiveRecordMutationBundleCommitsAndReplaysAcrossDialects(t *testing.T) {
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
			siteName := "bundle-" + tc.driver + "-" + suffix
			saleType := "Kernel Sale " + suffix
			paymentType := "Kernel Payment " + suffix
			sale := &doctype.DocType{Name: saleType, Fields: []doctype.Field{
				{Fieldname: "title", Fieldtype: "Data", Reqd: true},
				{Fieldname: "total_amount", Fieldtype: "Float", Reqd: true},
			}}
			payment := &doctype.DocType{Name: paymentType, Fields: []doctype.Field{
				{Fieldname: "sales_invoice", Fieldtype: "Link", Options: saleType, Reqd: true},
				{Fieldname: "amount", Fieldtype: "Float", Reqd: true},
				{Fieldname: "method", Fieldtype: "Select", Options: "Cash\nCard", Reqd: true},
			}}
			registry := doctype.NewRegistry()
			registry.LoadFull([]*doctype.DocType{sale, payment}, []*doctype.Role{{Name: doctype.AdminRole}}, []*doctype.Permission{
				{Doctype: saleType, Role: doctype.AdminRole, Read: true, Create: true, Write: true, Delete: true},
				{Doctype: paymentType, Role: doctype.AdminRole, Read: true, Create: true, Write: true, Delete: true},
			})
			cleanupKernelDialectRecords(t, database, dialect, siteName, saleType, paymentType)
			if err := schema.MigrateSiteFromRegistry(database, siteName, registry, dialect); err != nil {
				t.Fatalf("migrate %s bundle DocTypes: %v", tc.driver, err)
			}

			writer := outbox.NewSQLWriter(dialect)
			handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: dialect, Outbox: writer, SiteName: siteName})
			handler.SiteOutboxes = map[string]outbox.Writer{siteName: writer}
			ctx := kernelAdapterContext(t, database, registry, siteName, tc.driver)
			saleData, _ := json.Marshal(map[string]any{"title": "Dialect bundle sale", "total_amount": 500})
			paymentData, _ := json.Marshal(map[string]any{"sales_invoice": "$records.sale.name", "amount": 500, "method": "Cash"})
			payload := kernel.RecordMutationBundlePayload{Doctype: saleType, Records: []kernel.RecordMutationBundleItem{
				{Key: "sale", Doctype: saleType, Data: saleData},
				{Key: "payment", Doctype: paymentType, Data: paymentData},
			}}
			failurePayload := kernel.RecordMutationBundlePayload{Doctype: saleType, Records: []kernel.RecordMutationBundleItem{
				{Key: "sale", Doctype: saleType, Data: json.RawMessage(`{"title":"Must roll back","total_amount":500}`)},
				{Key: "payment", Doctype: paymentType, Data: paymentData},
			}}
			invalidReferencePayload := kernel.RecordMutationBundlePayload{Doctype: saleType, Records: []kernel.RecordMutationBundleItem{
				{Key: "sale", Doctype: saleType, Data: json.RawMessage(`{"title":"Invalid link sale","total_amount":500}`)},
				{Key: "payment", Doctype: paymentType, Data: json.RawMessage(`{"sales_invoice":"$records.missing.name","amount":500,"method":"Cash"}`)},
			}}
			if _, _, commandErr := handler.runKernelMutationBundle(ctx, invalidReferencePayload, "invalid-reference-"+suffix); commandErr == nil || commandErr.Type != contract.CodeValidationFailed {
				t.Fatalf("%s unresolved bundle reference error=%v, want validation failure", tc.driver, commandErr)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(sale.RawTableName())+" WHERE title = ?", "Invalid link sale"); got != 0 {
				t.Fatalf("%s invalid bundle reference left %d sale rows", tc.driver, got)
			}
			for label, query := range map[string]string{
				"completed audit": "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND status = 'completed'",
				"outbox":          "SELECT COUNT(*) FROM _kora_outbox WHERE site = ?",
				"receipt":         "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND idempotency_key = ?",
			} {
				args := []any{siteName}
				if label == "receipt" {
					args = append(args, "invalid-reference-"+suffix)
				}
				if got := countDialectRows(t, database, dialect, query, args...); got != 0 {
					t.Fatalf("%s invalid bundle left %d %s rows", tc.driver, got, label)
				}
			}
			dropFailureTrigger := installPaymentFailureTrigger(t, database, dialect, payment, suffix)
			ctx = kernelAdapterContext(t, database, registry, siteName, tc.driver)
			if _, _, commandErr := handler.runKernelMutationBundle(ctx, failurePayload, "failed-pos-bundle-"+suffix); commandErr == nil {
				t.Fatalf("%s injected payment failure unexpectedly committed", tc.driver)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(sale.RawTableName())+" WHERE title = ?", "Must roll back"); got != 0 {
				t.Fatalf("%s failed bundle left %d sale rows", tc.driver, got)
			}
			for table, query := range map[string]string{
				"completed audit": "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND status = 'completed'",
				"outbox":          "SELECT COUNT(*) FROM _kora_outbox WHERE site = ?",
				"receipt":         "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND idempotency_key = ?",
			} {
				args := []any{siteName}
				if table == "receipt" {
					args = append(args, "failed-pos-bundle-"+suffix)
				}
				if got := countDialectRows(t, database, dialect, query, args...); got != 0 {
					t.Fatalf("%s failed bundle left %d %s rows", tc.driver, got, table)
				}
			}
			dropFailureTrigger()
			const idempotencyKey = "cross-dialect-pos-bundle-" + "stable"
			first, replayed, commandErr := handler.runKernelMutationBundle(ctx, payload, idempotencyKey)
			if commandErr != nil {
				t.Fatalf("run %s POS bundle: %s: %s", tc.driver, commandErr.Type, commandErr.Message)
			}
			if replayed || first == nil || first.Name == "" || len(first.Related) != 1 || first.Related[0].Doctype != paymentType || first.Related[0].Name == "" {
				t.Fatalf("unexpected %s bundle result: replayed=%v result=%+v", tc.driver, replayed, first)
			}
			ctx = kernelAdapterContext(t, database, registry, siteName, tc.driver)
			second, replayed, commandErr := handler.runKernelMutationBundle(ctx, payload, idempotencyKey)
			if commandErr != nil || !replayed || second == nil || second.Name != first.Name || len(second.Related) != 1 || second.Related[0].Name != first.Related[0].Name {
				t.Fatalf("%s bundle replay mismatch: replayed=%v result=%+v err=%+v", tc.driver, replayed, second, commandErr)
			}

			var saleCount, paymentCount, auditCount, outboxCount, receiptCount int
			queryCount := func(query string, args ...any) int {
				t.Helper()
				var count int
				if err := database.QueryRow(kdb.Rebind(dialect, query), args...).Scan(&count); err != nil {
					t.Fatalf("count %s state on %s: %v", tc.driver, query, err)
				}
				return count
			}
			saleCount = queryCount("SELECT COUNT(*) FROM "+dialect.QuoteIdent(sale.RawTableName())+" WHERE name = ?", first.Name)
			paymentCount = queryCount("SELECT COUNT(*) FROM "+dialect.QuoteIdent(payment.RawTableName())+" WHERE name = ? AND sales_invoice = ?", first.Related[0].Name, first.Name)
			auditCount = queryCount("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND operation_id = ? AND status = 'completed'", siteName, first.Operation)
			outboxCount = queryCount("SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", siteName)
			receiptCount = queryCount("SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND idempotency_key = ?", siteName, idempotencyKey)
			if saleCount != 1 || paymentCount != 1 || auditCount != 2 || outboxCount != 2 || receiptCount != 1 {
				t.Fatalf("%s bundle persisted sale=%d payment=%d audit=%d outbox=%d receipts=%d; want 1/1/2/2/1", tc.driver, saleCount, paymentCount, auditCount, outboxCount, receiptCount)
			}

			// Race two identical related-write bundles against each live dialect.
			// The barrier ensures both requests contend on the same idempotency key.
			raceKey := "cross-dialect-race-" + suffix
			racePayload := kernel.RecordMutationBundlePayload{Doctype: saleType, Records: []kernel.RecordMutationBundleItem{
				{Key: "sale", Doctype: saleType, Data: json.RawMessage(`{"title":"Concurrent sale","total_amount":700}`)},
				{Key: "payment", Doctype: paymentType, Data: json.RawMessage(`{"sales_invoice":"$records.sale.name","amount":700,"method":"Card"}`)},
			}}
			start := make(chan struct{})
			type bundleResult struct {
				data     *kernel.ResultData
				replayed bool
				err      *contract.Error
			}
			results := make(chan bundleResult, 2)
			for range 2 {
				ctx := kernelAdapterContext(t, database, registry, siteName, tc.driver)
				go func() {
					<-start
					data, wasReplayed, err := handler.runKernelMutationBundle(ctx, racePayload, raceKey)
					results <- bundleResult{data: data, replayed: wasReplayed, err: err}
				}()
			}
			close(start)
			firstRace, secondRace := <-results, <-results
			for _, outcome := range []bundleResult{firstRace, secondRace} {
				if outcome.err != nil || outcome.data == nil || outcome.data.Name == "" || len(outcome.data.Related) != 1 {
					t.Fatalf("%s concurrent bundle failed: result=%+v err=%v", tc.driver, outcome.data, outcome.err)
				}
			}
			if firstRace.data.Name != secondRace.data.Name || firstRace.data.Related[0].Name != secondRace.data.Related[0].Name || firstRace.replayed == secondRace.replayed {
				t.Fatalf("%s concurrent bundle results differ or lack one replay: %+v / %+v", tc.driver, firstRace, secondRace)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(sale.RawTableName())+" WHERE title = ?", "Concurrent sale"); got != 1 {
				t.Fatalf("%s concurrent sale rows=%d, want exactly one", tc.driver, got)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(payment.RawTableName())+" WHERE sales_invoice = ?", firstRace.data.Name); got != 1 {
				t.Fatalf("%s concurrent payment rows=%d, want exactly one", tc.driver, got)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND idempotency_key = ?", siteName, raceKey); got != 1 {
				t.Fatalf("%s concurrent bundle receipts=%d, want exactly one", tc.driver, got)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND operation_id = ? AND status = 'completed'", siteName, firstRace.data.Operation); got != 2 {
				t.Fatalf("%s concurrent bundle completed audit rows=%d, want two", tc.driver, got)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id IN (?, ?)", siteName, firstRace.data.Name, firstRace.data.Related[0].Name); got != 2 {
				t.Fatalf("%s concurrent bundle outbox rows=%d, want two", tc.driver, got)
			}
		})
	}
}

func TestLiveExpectedVersionUpdateRaceAcrossDialects(t *testing.T) {
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
			siteName := "version-race-" + tc.driver + "-" + suffix
			doctypeName := "Kernel Version Race " + suffix
			dt := &doctype.DocType{Name: doctypeName, Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}}}
			registry := doctype.NewRegistry()
			registry.LoadFull([]*doctype.DocType{dt}, []*doctype.Role{{Name: doctype.AdminRole}}, []*doctype.Permission{
				{Doctype: doctypeName, Role: doctype.AdminRole, Read: true, Create: true, Write: true},
			})
			cleanupKernelDialectRecords(t, database, dialect, siteName, doctypeName)
			if err := schema.MigrateSiteFromRegistry(database, siteName, registry, dialect); err != nil {
				t.Fatalf("migrate %s version-race DocType: %v", tc.driver, err)
			}

			writer := outbox.NewSQLWriter(dialect)
			k := kernel.New(dialect, writer)
			k.TxManager = &orm.TxManager{DB: database, Registry: registry, Dialect: dialect, Outbox: writer, SiteName: siteName}
			baseContext := kernel.OperationContext{
				Site: siteName, User: "version-race@example.test", Roles: []string{doctype.AdminRole},
				Actor: contract.ActorContext{PrincipalID: "version-race@example.test", PrincipalType: contract.PrincipalHuman, Site: siteName, AuthenticatedAt: time.Now()},
			}
			createData, _ := json.Marshal(map[string]any{"title": "initial"})
			createPayload, _ := json.Marshal(kernel.RecordCreatePayload{Doctype: doctypeName, Data: createData})
			created, createErr := k.Execute(t.Context(), database, registry, kernel.Operation{Context: baseContext, Command: kernel.CommandRecordCreate, Payload: createPayload})
			if createErr != nil || created.Error != nil || created.Status != contract.StatusCompleted {
				t.Fatalf("create versioned record: result=%+v error=%+v", created, createErr)
			}
			var record kernel.ResultData
			if err := json.Unmarshal(created.Data, &record); err != nil {
				t.Fatalf("decode create result: %v", err)
			}
			var current any
			query := "SELECT modified FROM " + dialect.QuoteIdent(dt.RawTableName()) + " WHERE name = ?"
			if err := database.QueryRow(kdb.Rebind(dialect, query), record.Name).Scan(&current); err != nil {
				t.Fatalf("load current version: %v", err)
			}
			if current == nil {
				t.Fatal("record has no current version")
			}
			expected := kernel.CanonicalVersion(current)

			start := make(chan struct{})
			type outcome struct {
				title  string
				result contract.CommandResult
				err    *contract.Error
			}
			results := make(chan outcome, 2)
			for _, title := range []string{"first contender", "second contender"} {
				title := title
				updateData, _ := json.Marshal(map[string]any{"title": title})
				updatePayload, _ := json.Marshal(kernel.RecordUpdatePayload{Doctype: doctypeName, Name: record.Name, Data: updateData})
				go func() {
					<-start
					ctx := baseContext
					ctx.ExpectedVersion = expected
					result, err := k.Execute(t.Context(), database, registry, kernel.Operation{Context: ctx, Command: kernel.CommandRecordUpdate, Payload: updatePayload})
					results <- outcome{title: title, result: result, err: err}
				}()
			}
			close(start)

			successes, conflicts := 0, 0
			for range 2 {
				got := <-results
				if got.err != nil {
					if got.err.Type == contract.CodeConflict {
						conflicts++
						continue
					}
					t.Fatalf("%s update: %v", tc.driver, got.err)
				}
				if got.result.Status == contract.StatusCompleted && got.result.Error == nil {
					successes++
					continue
				}
				if got.result.Error != nil && got.result.Error.Type == contract.CodeConflict {
					conflicts++
					continue
				}
				t.Fatalf("unexpected %s update result: %+v", tc.driver, got.result)
			}
			if successes != 1 || conflicts != 1 {
				t.Fatalf("%s expected-version race: successes=%d conflicts=%d, want one each", tc.driver, successes, conflicts)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND command_name = 'record.update' AND status = 'completed'", siteName); got != 1 {
				t.Fatalf("%s completed update audits=%d, want 1", tc.driver, got)
			}
			var finalTitle string
			if err := database.QueryRow(kdb.Rebind(dialect, "SELECT title FROM "+dialect.QuoteIdent(dt.RawTableName())+" WHERE name = ?"), record.Name).Scan(&finalTitle); err != nil {
				t.Fatalf("load final record: %v", err)
			}
			if finalTitle != "first contender" && finalTitle != "second contender" {
				t.Fatalf("%s final title=%q; no contender won", tc.driver, finalTitle)
			}
		})
	}
}

func TestLiveWorkflowTransitionUsesKernelAcrossDialects(t *testing.T) {
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
			siteName := "workflow-" + tc.driver + "-" + suffix
			doctypeName := "Kernel Workflow " + suffix
			dt := &doctype.DocType{Name: doctypeName, Fields: []doctype.Field{
				{Fieldname: "title", Fieldtype: "Data", Reqd: true},
				{Fieldname: "status", Fieldtype: "Select", Options: "Draft\nDone", Reqd: true},
			}}
			registry := doctype.NewRegistry()
			registry.LoadFull([]*doctype.DocType{dt}, []*doctype.Role{{Name: doctype.AdminRole}, {Name: "Reader"}}, []*doctype.Permission{{
				Doctype: doctypeName, Role: doctype.AdminRole, Read: true, Create: true, Write: true, Delete: true, Submit: true,
			}, {
				Doctype: doctypeName, Role: "Reader", Read: true,
			}})
			registry.Workflows.Register(&doctype.Workflow{
				Name: "Kernel Workflow " + suffix, DocumentType: doctypeName, IsActive: true, WorkflowStateField: "status",
				States:      []doctype.WorkflowState{{State: "Draft", DocStatus: 0}, {State: "Done", DocStatus: 1}},
				Transitions: []doctype.WorkflowTransition{{Action: "Complete", From: "Draft", To: "Done", Allowed: doctype.AdminRole}},
			})
			cleanupKernelDialectRecords(t, database, dialect, siteName, doctypeName)
			if err := schema.MigrateSiteFromRegistry(database, siteName, registry, dialect); err != nil {
				t.Fatalf("migrate %s workflow DocType: %v", tc.driver, err)
			}
			writer := outbox.NewSQLWriter(dialect)
			handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: dialect, Outbox: writer, SiteName: siteName})
			handler.SiteOutboxes = map[string]outbox.Writer{siteName: writer}
			path := "/api/resource/" + url.PathEscape(doctypeName)
			created := serveResourceMutationBodyWithDBType(t, handler, database, registry, siteName,
				httpMethodPost, path, `{"title":"Workflow test","status":"Draft"}`, "", []string{doctype.AdminRole}, tc.driver)
			if created.Code != http.StatusCreated {
				t.Fatalf("%s workflow seed create returned HTTP %d: %s", tc.driver, created.Code, created.Body.String())
			}
			createdDoc := requireRESTDocument(t, created.Body.Bytes(), doctypeName)
			name, ok := createdDoc["name"].(string)
			if !ok || name == "" {
				t.Fatalf("%s workflow seed omitted document name: %#v", tc.driver, createdDoc)
			}
			ctx := kernelAdapterContext(t, database, registry, siteName, tc.driver)
			ctx.Set("user_role", "Reader")
			ctx.Set("user_roles", []string{"Reader"})
			if _, denied := handler.runKernelWorkflowTransition(ctx, doctypeName, name, "Complete", 0); denied == nil {
				t.Fatalf("%s workflow unexpectedly accepted a role without submit permission", tc.driver)
			}
			ctx = kernelAdapterContext(t, database, registry, siteName, tc.driver)
			if _, invalid := handler.runKernelWorkflowTransition(ctx, doctypeName, name, "Archive", 0); invalid == nil {
				t.Fatalf("%s workflow unexpectedly accepted an unavailable transition", tc.driver)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(dt.RawTableName())+" WHERE name = ? AND status = 'Draft'", name); got != 1 {
				t.Fatalf("%s rejected workflow calls changed the document", tc.driver)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doc_name = ? AND command_name = ? AND status = 'completed'", siteName, name, kernel.CommandRecordWorkflowTransition); got != 0 {
				t.Fatalf("%s rejected workflow calls wrote %d completed transition audits", tc.driver, got)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ?", siteName, name); got != 1 {
				t.Fatalf("%s rejected workflow calls changed outbox rows to %d; want only the create event", tc.driver, got)
			}
			ctx = kernelAdapterContext(t, database, registry, siteName, tc.driver)
			updated, commandErr := handler.runKernelWorkflowTransition(ctx, doctypeName, name, "Complete", 0)
			if commandErr != nil {
				t.Fatalf("%s workflow transition failed: %s: %s", tc.driver, commandErr.Type, commandErr.Message)
			}
			if updated.Name != name || updated.GetString("status") != "Done" || updated.DocStatus != 1 {
				t.Fatalf("%s workflow response = name:%q status:%q doc_status:%d", tc.driver, updated.Name, updated.GetString("status"), updated.DocStatus)
			}
			var state string
			var docStatus, auditCount, outboxCount int
			if err := database.QueryRow(kdb.Rebind(dialect, "SELECT status, doc_status FROM "+dialect.QuoteIdent(dt.RawTableName())+" WHERE name = ?"), name).Scan(&state, &docStatus); err != nil {
				t.Fatalf("read %s workflow record: %v", tc.driver, err)
			}
			if err := database.QueryRow(kdb.Rebind(dialect, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doc_name = ? AND command_name = ? AND status = 'completed'"), siteName, name, kernel.CommandRecordWorkflowTransition).Scan(&auditCount); err != nil {
				t.Fatalf("count %s workflow audit: %v", tc.driver, err)
			}
			if err := database.QueryRow(kdb.Rebind(dialect, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ?"), siteName, name).Scan(&outboxCount); err != nil {
				t.Fatalf("count %s workflow outbox: %v", tc.driver, err)
			}
			if state != "Done" || docStatus != 1 || auditCount != 1 || outboxCount != 2 {
				t.Fatalf("%s workflow persisted state=%q doc_status=%d audit=%d outbox=%d; want Done/1/1/2", tc.driver, state, docStatus, auditCount, outboxCount)
			}
		})
	}
}

func TestScriptProviderCRUDUsesKernelAcrossDialects(t *testing.T) {
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
			siteName := "script-provider-" + tc.driver + "-" + suffix
			dt := &doctype.DocType{Name: "Script Record " + suffix, Fields: []doctype.Field{
				{Fieldname: "title", Fieldtype: "Data", Reqd: true},
			}}
			registry := doctype.NewRegistry()
			registry.LoadFull([]*doctype.DocType{dt}, []*doctype.Role{{Name: doctype.AdminRole}}, []*doctype.Permission{{
				Doctype: dt.Name, Role: doctype.AdminRole, Read: true, Create: true, Write: true, Delete: true,
			}})
			cleanupKernelDialectRecords(t, database, dialect, siteName, dt.Name)
			if err := schema.MigrateSiteFromRegistry(database, siteName, registry, dialect); err != nil {
				t.Fatalf("migrate %s script DocType: %v", tc.driver, err)
			}
			writer := outbox.NewSQLWriter(dialect)
			tx := &orm.TxManager{
				DB: database, Registry: registry, Dialect: dialect, Outbox: writer, SiteName: siteName,
				CurrentUser: "script@example.test", CurrentUserRole: doctype.AdminRole, CurrentUserRoles: []string{doctype.AdminRole},
			}
			provider := NewScriptProvider(tx, registry, siteName, nil, nil)
			created, err := provider.CreateDoc(dt.Name, map[string]any{"title": "script create", "unlisted": "not stored"}, "owner@example.test", "script@example.test")
			if err != nil {
				t.Fatalf("%s script create: %v", tc.driver, err)
			}
			name, _ := created["name"].(string)
			if name == "" || created["title"] != "script create" || created["unlisted"] != "not stored" {
				t.Fatalf("%s script create response = %#v", tc.driver, created)
			}
			if err := provider.SaveDoc(dt.Name, map[string]any{"name": name, "title": "script update", "unlisted": "discarded"}, "script@example.test"); err != nil {
				t.Fatalf("%s script update: %v", tc.driver, err)
			}
			loaded, err := provider.GetDoc(dt.Name, name)
			if err != nil || loaded["title"] != "script update" || loaded["unlisted"] != nil {
				t.Fatalf("%s script read after update = %#v, err=%v", tc.driver, loaded, err)
			}
			if err := provider.DeleteDoc(dt.Name, name); err != nil {
				t.Fatalf("%s script delete: %v", tc.driver, err)
			}
			query := func(statement string, args ...any) int {
				t.Helper()
				var count int
				if err := database.QueryRow(kdb.Rebind(dialect, statement), args...).Scan(&count); err != nil {
					t.Fatalf("query %s script state: %v", tc.driver, err)
				}
				return count
			}
			if got := query("SELECT COUNT(*) FROM "+dialect.QuoteIdent(dt.RawTableName())+" WHERE name = ?", name); got != 0 {
				t.Fatalf("%s script delete left %d rows", tc.driver, got)
			}
			if got := query("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doc_name = ? AND status = 'completed' AND command_name IN ('record.create', 'record.update', 'record.delete')", siteName, name); got != 3 {
				t.Fatalf("%s script mutation audit rows = %d, want 3", tc.driver, got)
			}
			if got := query("SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ?", siteName, name); got != 3 {
				t.Fatalf("%s script mutation outbox rows = %d, want 3", tc.driver, got)
			}
		})
	}
}

func TestExternalOperationManifestActionUsesKernelAcrossDialects(t *testing.T) {
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
			siteName := "external-operation-" + tc.driver + "-" + fmt.Sprintf("%x", time.Now().UnixNano())
			registry := doctype.NewRegistry()
			registerPOSIntegrationTypes(t, database, siteName, registry, dialect)
			cleanupKernelDialectRecords(t, database, dialect, siteName, "Sale Item", "Sale", "Payment", "External Operation", "External Operation Event")
			if tc.driver != "postgres" {
				t.Cleanup(func() { _, _ = database.Exec("DROP TABLE IF EXISTS " + dialect.QuoteIdent("tabSale__items")) })
			}
			writer := outbox.NewSQLWriter(dialect)
			handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: dialect, Outbox: writer, SiteName: siteName})
			handler.SiteOutboxes = map[string]outbox.Writer{siteName: writer}
			registry.Views.Register(&doctype.View{
				Name: "Payment initiation", Route: "/payment", Type: "register", SourceDocType: "Sale",
				Components: []doctype.ViewComponent{{ID: "payment", Actions: []doctype.ViewAction{{
					ID: "initiate", Trigger: "on_click", Type: "initiate_external_operation",
					Config: map[string]any{"operation_type": "Payment", "purpose": "POS sale payment", "source_doctype": "Sale", "provider": "M-Pesa"},
				}}}},
			})
			request := map[string]any{"client_reference": "external-op-" + siteName, "total": 250.0, "payment_method": "mpesa", "customer_phone": "+254700000001"}
			first := serveManifestAction(t, handler, siteName, database, registry, "initiate", request, tc.driver)
			if first.Code != http.StatusOK {
				t.Fatalf("%s initiate status = %d: %s", tc.driver, first.Code, first.Body.String())
			}
			operation := requireActionDocument(t, first.Body.Bytes())
			operationName, _ := operation["name"].(string)
			if operationName == "" || operation["status"] != "Pending" || operation["idempotency_key"] != request["client_reference"] {
				t.Fatalf("%s initiation response = %#v", tc.driver, operation)
			}
			retry := serveManifestAction(t, handler, siteName, database, registry, "initiate", request, tc.driver)
			if retry.Code != http.StatusOK {
				t.Fatalf("%s initiation retry status = %d: %s", tc.driver, retry.Code, retry.Body.String())
			}
			retried := requireActionDocument(t, retry.Body.Bytes())
			if retried["name"] != operationName {
				t.Fatalf("%s retry returned operation %v, want %s", tc.driver, retried["name"], operationName)
			}
			for table, statement := range map[string]string{
				"payment": "SELECT COUNT(*) FROM " + dialect.QuoteIdent("tabPayment") + " WHERE external_operation = ? AND status = 'Pending' AND amount = ?",
				"event":   "SELECT COUNT(*) FROM " + dialect.QuoteIdent("tabExternal Operation Event") + " WHERE operation = ? AND event_type = 'Initiate'",
				"audit":   "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND operation_id = (SELECT operation_id FROM _kora_operation_audit WHERE site = ? AND doctype = 'External Operation' AND doc_name = ? ORDER BY created_at LIMIT 1) AND status = 'completed'",
			} {
				args := []any{operationName, 250.0}
				want := 1
				if table == "event" {
					args = []any{operationName}
				}
				if table == "audit" {
					args = []any{siteName, siteName, operationName}
					want = 3
				}
				got := countDialectRows(t, database, dialect, statement, args...)
				if got != want {
					t.Fatalf("%s initiation %s rows=%d, want %d", tc.driver, table, got, want)
				}
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", siteName); got != 3 {
				t.Fatalf("%s initiation outbox rows=%d, want one event for each bundled record", tc.driver, got)
			}
			if got := countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND status = 'completed'", siteName); got != 3 {
				t.Fatalf("%s initiation completed audit rows=%d, want 3", tc.driver, got)
			}
		})
	}
}

func installPaymentFailureTrigger(t *testing.T, database *sql.DB, dialect kdb.Dialect, payment *doctype.DocType, suffix string) func() {
	t.Helper()
	table := dialect.QuoteIdent(payment.RawTableName())
	trigger := dialect.QuoteIdent("kora_fail_payment_" + suffix)
	var drop func()
	switch dialect.DriverName() {
	case "postgres":
		function := dialect.QuoteIdent("kora_fail_payment_fn_" + suffix)
		if _, err := database.Exec(`CREATE FUNCTION ` + function + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced payment failure'; END; $$`); err != nil {
			t.Fatalf("create PostgreSQL failure function: %v", err)
		}
		if _, err := database.Exec(`CREATE TRIGGER ` + trigger + ` BEFORE INSERT ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION ` + function + `()`); err != nil {
			t.Fatalf("create PostgreSQL failure trigger: %v", err)
		}
		drop = func() {
			_, _ = database.Exec(`DROP TRIGGER IF EXISTS ` + trigger + ` ON ` + table)
			_, _ = database.Exec(`DROP FUNCTION IF EXISTS ` + function + `()`)
		}
	case "libsql":
		if _, err := database.Exec(`CREATE TRIGGER ` + trigger + ` BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT, 'forced payment failure'); END`); err != nil {
			t.Fatalf("create LibSQL failure trigger: %v", err)
		}
		drop = func() { _, _ = database.Exec(`DROP TRIGGER IF EXISTS ` + trigger) }
	default:
		t.Fatalf("unsupported test driver for failure trigger: %s", dialect.DriverName())
	}
	t.Cleanup(drop)
	return drop
}

func countDialectRows(t *testing.T, database *sql.DB, dialect kdb.Dialect, query string, args ...any) int {
	t.Helper()
	var count int
	if err := database.QueryRow(kdb.Rebind(dialect, query), args...).Scan(&count); err != nil {
		t.Fatalf("count rows on %s: %v", dialect.DriverName(), err)
	}
	return count
}

func openKernelDialectDatabase(t *testing.T, driver, dsn string) (*sql.DB, kdb.Dialect) {
	t.Helper()
	dialect := kdb.Resolve(driver)
	if driver != "postgres" {
		database, err := sql.Open(driver, dsn)
		if err != nil {
			t.Fatalf("open %s database: %v", driver, err)
		}
		database.SetMaxOpenConns(4)
		t.Cleanup(func() { _ = database.Close() })
		if err := database.Ping(); err != nil {
			t.Fatalf("ping %s database: %v", driver, err)
		}
		return database, dialect
	}

	adminDB, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("open PostgreSQL database: %v", err)
	}
	if err := adminDB.Ping(); err != nil {
		_ = adminDB.Close()
		t.Fatalf("ping PostgreSQL database: %v", err)
	}
	schemaName := fmt.Sprintf("kora_bundle_%x", time.Now().UnixNano())
	if _, err := adminDB.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		_ = adminDB.Close()
		t.Fatalf("create isolated PostgreSQL schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminDB.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`)
		_ = adminDB.Close()
	})
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse PostgreSQL DSN: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schemaName)
	parsed.RawQuery = query.Encode()
	database, err := sql.Open(driver, parsed.String())
	if err != nil {
		t.Fatalf("open isolated PostgreSQL schema: %v", err)
	}
	database.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Ping(); err != nil {
		t.Fatalf("ping isolated PostgreSQL schema: %v", err)
	}
	return database, dialect
}

func cleanupKernelDialectRecords(t *testing.T, database *sql.DB, dialect kdb.Dialect, site string, doctypes ...string) {
	t.Helper()
	if _, ok := dialect.(*kdb.PostgresDialect); ok {
		return // the complete isolated schema is removed by the database fixture cleanup.
	}
	t.Cleanup(func() {
		for _, name := range doctypes {
			for _, table := range []string{"_kora_field", "_kora_permission"} {
				column := "parent"
				if table == "_kora_permission" {
					column = "doctype"
				}
				_, _ = database.Exec(kdb.Rebind(dialect, "DELETE FROM "+dialect.QuoteIdent(table)+" WHERE "+dialect.QuoteIdent(column)+" = ?"), name)
			}
			_, _ = database.Exec(kdb.Rebind(dialect, "DELETE FROM _kora_doctype WHERE name = ?"), name)
			_, _ = database.Exec(kdb.Rebind(dialect, "DELETE FROM _kora_naming_series WHERE doctype = ?"), name)
			_, _ = database.Exec("DROP TABLE " + dialect.QuoteIdent("tab"+name))
		}
		for _, table := range []string{"_kora_operation_audit", "_kora_idempotency_receipt", "_kora_outbox"} {
			_, _ = database.Exec(kdb.Rebind(dialect, "DELETE FROM "+dialect.QuoteIdent(table)+" WHERE site = ?"), site)
		}
	})
}
