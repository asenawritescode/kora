//go:build integration

package api

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/script"
	ksite "github.com/asenawritescode/kora/site"
)

func TestExternalOperationProviderScriptsUseScriptStoreAcrossDialects(t *testing.T) {
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
				t.Fatalf("bootstrap %s system tables: %v", tc.driver, err)
			}
			var extensibilityTables []string
			switch tc.driver {
			case "postgres":
				extensibilityTables = kdb.ExtensibilityTablesPostgres()
			case "libsql":
				extensibilityTables = kdb.ExtensibilityTablesLibSQL()
			}
			for _, statement := range extensibilityTables {
				if !strings.Contains(statement, "_kora_script") || strings.Contains(statement, "_kora_extension") {
					continue
				}
				if _, err := database.Exec(statement); err != nil {
					t.Fatalf("create %s script tables: %v", tc.driver, err)
				}
			}

			siteName := fmt.Sprintf("provider-script-%s-%x", tc.driver, time.Now().UnixNano())
			registry := resourceIntegrationRegistry()
			registerPOSIntegrationTypes(t, database, siteName, registry, dialect)
			cleanupKernelDialectRecords(t, database, dialect, siteName, "Sale Item", "Sale", "Payment", "External Operation", "External Operation Event")
			if tc.driver != "postgres" {
				t.Cleanup(func() { _, _ = database.Exec("DROP TABLE IF EXISTS " + dialect.QuoteIdent("tabSale__items")) })
			}
			store := &script.Store{DB: database, Dialect: dialect}
			if err := store.Insert(script.ScriptRecord{
				Name: "provider-fails", Site: siteName, ScriptType: script.TypeAPIMethod,
				DocType: "External Operation", Event: script.EventPayment, MethodPath: "provider.test", IsActive: true,
			}); err != nil {
				t.Fatalf("insert provider script on %s: %v", tc.driver, err)
			}
			for _, rec := range []script.ScriptRecord{
				{Name: "hook-portability", Site: siteName, ScriptType: script.TypeDocEvent, DocType: "Unrelated DocType", Event: script.EventValidate, IsActive: true},
				{Name: "workflow-portability", Site: siteName, ScriptType: script.TypeWorkflowAction, WorkflowAction: "Approve", IsActive: true},
			} {
				if err := store.Insert(rec); err != nil {
					t.Fatalf("insert %s script on %s: %v", rec.Name, tc.driver, err)
				}
			}
			if active, err := store.LoadActiveScripts(siteName, "Unrelated DocType", script.EventValidate); err != nil || len(active) != 1 {
				t.Fatalf("load active scripts on %s: count=%d err=%v", tc.driver, len(active), err)
			}
			if method, err := store.LoadMethodScript(siteName, "provider.test"); err != nil || method == nil || method.Name != "provider-fails" {
				t.Fatalf("load method script on %s: script=%#v err=%v", tc.driver, method, err)
			}
			if actions, err := store.LoadWorkflowActionScripts(siteName, "Approve"); err != nil || len(actions) != 1 {
				t.Fatalf("load workflow scripts on %s: count=%d err=%v", tc.driver, len(actions), err)
			}
			if all, err := store.LoadAllForSite(siteName); err != nil || len(all) != 3 {
				t.Fatalf("load all scripts on %s: count=%d err=%v", tc.driver, len(all), err)
			}
			handler := NewHandler(registry, nil)
			handler.SiteScriptStores = map[string]*script.Store{siteName: store}
			handler.ScriptRunner = failingOperationScriptRunner{}
			registry.Views.Register(externalOperationScriptView("provider-fails"))

			request := map[string]any{
				"client_reference": "provider-failure-" + siteName,
				"total":            250.0, "payment_method": "mpesa", "customer_phone": "+254700000001",
			}
			response := serveManifestAction(t, handler, siteName, database, registry, "initiate-script", request, tc.driver)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("provider failure status on %s = %d: %s", tc.driver, response.Code, response.Body.String())
			}
			var operationName, operationStatus, paymentStatus string
			if err := database.QueryRow(kdb.Rebind(dialect, "SELECT name, status FROM \"tabExternal Operation\" WHERE idempotency_key = ?"), request["client_reference"]).Scan(&operationName, &operationStatus); err != nil {
				t.Fatalf("load failed operation on %s: %v; response=%d %s", tc.driver, err, response.Code, response.Body.String())
			}
			if err := database.QueryRow(kdb.Rebind(dialect, "SELECT status FROM \"tabPayment\" WHERE external_operation = ?"), operationName).Scan(&paymentStatus); err != nil {
				t.Fatalf("load failed payment on %s: %v", tc.driver, err)
			}
			var executionCount int
			if err := database.QueryRow(kdb.Rebind(dialect, "SELECT COUNT(*) FROM _kora_script_execution WHERE site = ? AND script_name = ? AND status = 'error'"), siteName, "provider-fails").Scan(&executionCount); err != nil {
				t.Fatalf("count provider execution on %s: %v", tc.driver, err)
			}
			if operationStatus != "Failed" || paymentStatus != "Failed" || executionCount != 1 {
				t.Fatalf("provider failure state on %s: operation=%s payment=%s execution_rows=%d", tc.driver, operationStatus, paymentStatus, executionCount)
			}
			if executions, err := store.LoadExecutions(siteName, "provider-fails", 10); err != nil || len(executions) != 1 || executions[0]["status"] != "error" {
				t.Fatalf("load script executions on %s: rows=%#v err=%v", tc.driver, executions, err)
			}
			priority := 2
			if err := store.Update(siteName, "provider-fails", script.ScriptUpdateRequest{Priority: &priority}, "migration-test"); err != nil {
				t.Fatalf("update provider script on %s: %v", tc.driver, err)
			}
			updated, err := store.LoadByName(siteName, "provider-fails")
			if err != nil || updated == nil || updated.Priority != priority {
				t.Fatalf("load updated script on %s: script=%#v err=%v", tc.driver, updated, err)
			}
			if err := store.Delete(siteName, "provider-fails"); err != nil {
				t.Fatalf("delete provider script on %s: %v", tc.driver, err)
			}
			deleted, err := store.LoadByName(siteName, "provider-fails")
			if err != nil || deleted != nil {
				t.Fatalf("load deleted script on %s: script=%#v err=%v", tc.driver, deleted, err)
			}
			if err := store.Insert(script.ScriptRecord{
				Name: "provider-succeeds", Site: siteName, ScriptType: script.TypeAPIMethod,
				DocType: "External Operation", Event: script.EventPayment, MethodPath: "provider.success", IsActive: true,
			}); err != nil {
				t.Fatalf("insert successful provider script on %s: %v", tc.driver, err)
			}
			handler.ScriptRunner = fixedOperationScriptRunner{result: map[string]any{
				"success": true, "status": "Succeeded", "provider_reference": "RCP-" + tc.driver,
			}}
			registry.Views.Register(externalOperationScriptView("provider-succeeds"))
			request["client_reference"] = "provider-success-" + siteName
			successResponse := serveManifestAction(t, handler, siteName, database, registry, "initiate-script", request, tc.driver)
			if successResponse.Code != http.StatusOK {
				t.Fatalf("provider success status on %s = %d: %s", tc.driver, successResponse.Code, successResponse.Body.String())
			}
			successOperation, _ := requireActionDocument(t, successResponse.Body.Bytes())["name"].(string)
			var successOperationStatus, successPaymentStatus string
			if err := database.QueryRow(kdb.Rebind(dialect, "SELECT status FROM \"tabExternal Operation\" WHERE name = ?"), successOperation).Scan(&successOperationStatus); err != nil {
				t.Fatalf("load successful operation on %s: %v", tc.driver, err)
			}
			if err := database.QueryRow(kdb.Rebind(dialect, "SELECT status FROM \"tabPayment\" WHERE external_operation = ?"), successOperation).Scan(&successPaymentStatus); err != nil {
				t.Fatalf("load successful payment on %s: %v", tc.driver, err)
			}
			if successOperationStatus != "Succeeded" || successPaymentStatus != "Succeeded" {
				t.Fatalf("provider success state on %s: operation=%s payment=%s", tc.driver, successOperationStatus, successPaymentStatus)
			}
		})
	}
}

func externalOperationScriptView(scriptName string) *doctype.View {
	return &doctype.View{
		Name: "Payment initiation script", Route: "/payment-script", Type: "register", SourceDocType: "Sale",
		Components: []doctype.ViewComponent{{ID: "payment", Actions: []doctype.ViewAction{{
			ID: "initiate-script", Trigger: "on_click", Type: "initiate_external_operation",
			Config: map[string]any{"operation_type": "Payment", "purpose": "POS sale payment", "source_doctype": "Sale", "provider": "M-Pesa", "script": scriptName},
		}}}},
	}
}
