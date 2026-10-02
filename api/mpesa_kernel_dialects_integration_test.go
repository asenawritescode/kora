//go:build integration

package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/schema"
	ksite "github.com/asenawritescode/kora/site"
	"github.com/gin-gonic/gin"
)

// TestLiveMPesaCallbackBundleAcrossDialects proves the callback's existing HTTP
// behavior and the operation/payment/event atomic update on every live SQL
// dialect supported by the integration suite.
func TestLiveMPesaCallbackBundleAcrossDialects(t *testing.T) {
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
			siteName := "mpesa-" + tc.driver + "-" + suffix
			operation := &doctype.DocType{Name: "External Operation", Fields: []doctype.Field{
				{Fieldname: "operation_type", Fieldtype: "Data", Reqd: true},
				{Fieldname: "purpose", Fieldtype: "Data", Reqd: true},
				{Fieldname: "source_doctype", Fieldtype: "Data", Reqd: true},
				{Fieldname: "provider", Fieldtype: "Data", Reqd: true},
				{Fieldname: "status", Fieldtype: "Data", Reqd: true},
				{Fieldname: "provider_request_id", Fieldtype: "Data"},
				{Fieldname: "provider_reference", Fieldtype: "Data"},
				{Fieldname: "error_message", Fieldtype: "Text"},
				{Fieldname: "completed_at", Fieldtype: "Datetime"},
			}}
			payment := &doctype.DocType{Name: "Payment", Fields: []doctype.Field{
				{Fieldname: "external_operation", Fieldtype: "Link", Options: operation.Name},
				{Fieldname: "status", Fieldtype: "Data"},
				{Fieldname: "provider_reference", Fieldtype: "Data"},
			}}
			event := &doctype.DocType{Name: "External Operation Event", Fields: []doctype.Field{
				{Fieldname: "operation", Fieldtype: "Link", Options: operation.Name, Reqd: true},
				{Fieldname: "direction", Fieldtype: "Data"},
				{Fieldname: "event_type", Fieldtype: "Data"},
				{Fieldname: "provider", Fieldtype: "Data"},
				{Fieldname: "provider_reference", Fieldtype: "Data"},
				{Fieldname: "previous_status", Fieldtype: "Data"},
				{Fieldname: "new_status", Fieldtype: "Data"},
				{Fieldname: "request_payload", Fieldtype: "JSON"},
				{Fieldname: "response_payload", Fieldtype: "JSON"},
				{Fieldname: "processing_status", Fieldtype: "Data"},
				{Fieldname: "error_message", Fieldtype: "Text"},
				{Fieldname: "idempotency_key", Fieldtype: "Data", Unique: true},
				{Fieldname: "received_at", Fieldtype: "Datetime"},
				{Fieldname: "processed_at", Fieldtype: "Datetime"},
			}}
			registry := doctype.NewRegistry()
			permissions := make([]*doctype.Permission, 0, 3)
			for _, dt := range []*doctype.DocType{operation, payment, event} {
				permissions = append(permissions, &doctype.Permission{Doctype: dt.Name, Role: doctype.AdminRole, Read: true, Create: true, Write: true, Delete: true})
			}
			registry.LoadFull([]*doctype.DocType{operation, payment, event}, []*doctype.Role{{Name: doctype.AdminRole}}, permissions)
			cleanupKernelDialectRecords(t, database, dialect, siteName, operation.Name, payment.Name, event.Name)
			if err := schema.MigrateSiteFromRegistry(database, siteName, registry, dialect); err != nil {
				t.Fatalf("migrate %s M-Pesa DocTypes: %v", tc.driver, err)
			}
			writer := outbox.NewSQLWriter(dialect)
			handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: dialect, Outbox: writer, SiteName: siteName})
			handler.SiteOutboxes = map[string]outbox.Writer{siteName: writer}

			seed := func(query string, args ...any) {
				t.Helper()
				if _, err := database.Exec(kdb.Rebind(dialect, query), args...); err != nil {
					t.Fatalf("seed %s: %v", tc.driver, err)
				}
			}
			seed("INSERT INTO "+dialect.QuoteIdent(operation.RawTableName())+" (name, operation_type, purpose, source_doctype, provider, status, provider_request_id) VALUES (?, ?, ?, ?, ?, ?, ?)", "OP-"+suffix, "Payment", "POS payment", "Sale", "M-Pesa", "Pending", "checkout-"+suffix)
			seed("INSERT INTO "+dialect.QuoteIdent(payment.RawTableName())+" (name, external_operation, status) VALUES (?, ?, ?)", "PAY-"+suffix, "OP-"+suffix, "Pending")
			query := func(sql string, args ...any) int {
				t.Helper()
				var count int
				if err := database.QueryRow(kdb.Rebind(dialect, sql), args...).Scan(&count); err != nil {
					t.Fatalf("query %s state: %v", tc.driver, err)
				}
				return count
			}

			body := `{"Body":{"stkCallback":{"CheckoutRequestID":"checkout-` + suffix + `","ResultCode":0,"ResultDesc":"Success","CallbackMetadata":{"Item":[{"Name":"MpesaReceiptNumber","Value":"RCP-` + suffix + `"}]}}}}`
			dropFailureTrigger := installPaymentUpdateFailureTrigger(t, database, dialect, payment, suffix)
			failed := serveMPesaCallbackDialect(t, handler, database, registry, siteName, tc.driver, body)
			if failed.Code != http.StatusInternalServerError {
				t.Fatalf("%s callback with forced payment failure returned %d: %s", tc.driver, failed.Code, failed.Body.String())
			}
			if got := query("SELECT COUNT(*) FROM "+dialect.QuoteIdent(operation.RawTableName())+" WHERE name = ? AND status = 'Pending'", "OP-"+suffix); got != 1 {
				t.Fatalf("%s failed callback changed the operation", tc.driver)
			}
			if got := query("SELECT COUNT(*) FROM "+dialect.QuoteIdent(payment.RawTableName())+" WHERE name = ? AND status = 'Pending'", "PAY-"+suffix); got != 1 {
				t.Fatalf("%s failed callback changed the payment", tc.driver)
			}
			if got := query("SELECT COUNT(*) FROM "+dialect.QuoteIdent(event.RawTableName())+" WHERE operation = ?", "OP-"+suffix); got != 0 {
				t.Fatalf("%s failed callback left %d callback events", tc.driver, got)
			}
			if got := query("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ?", siteName); got != 0 {
				t.Fatalf("%s failed callback left %d completed operation audits", tc.driver, got)
			}
			if got := query("SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", siteName); got != 0 {
				t.Fatalf("%s failed callback left %d outbox rows", tc.driver, got)
			}
			dropFailureTrigger()
			response := serveMPesaCallbackDialect(t, handler, database, registry, siteName, tc.driver, body)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"payment_status":"Succeeded"`) {
				t.Fatalf("%s callback response = %d: %s", tc.driver, response.Code, response.Body.String())
			}
			retry := serveMPesaCallbackDialect(t, handler, database, registry, siteName, tc.driver, body)
			if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), `"status":"duplicate"`) {
				t.Fatalf("%s terminal retry = %d: %s", tc.driver, retry.Code, retry.Body.String())
			}

			var operationStatus, operationReference, paymentStatus, paymentReference string
			if err := database.QueryRow(kdb.Rebind(dialect, "SELECT status, provider_reference FROM "+dialect.QuoteIdent(operation.RawTableName())+" WHERE name = ?"), "OP-"+suffix).Scan(&operationStatus, &operationReference); err != nil {
				t.Fatal("read operation:", err)
			}
			if err := database.QueryRow(kdb.Rebind(dialect, "SELECT status, provider_reference FROM "+dialect.QuoteIdent(payment.RawTableName())+" WHERE name = ?"), "PAY-"+suffix).Scan(&paymentStatus, &paymentReference); err != nil {
				t.Fatal("read payment:", err)
			}
			if operationStatus != "Succeeded" || operationReference != "RCP-"+suffix || paymentStatus != operationStatus || paymentReference != operationReference {
				t.Fatalf("%s operation=(%s,%s) payment=(%s,%s), want matching success", tc.driver, operationStatus, operationReference, paymentStatus, paymentReference)
			}
			if got := query("SELECT COUNT(*) FROM "+dialect.QuoteIdent(event.RawTableName())+" WHERE operation = ? AND event_type = 'Callback'", "OP-"+suffix); got != 2 {
				t.Fatalf("%s callback evidence rows=%d, want processed plus duplicate", tc.driver, got)
			}
			if got := query("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND status = 'completed'", siteName); got != 4 {
				t.Fatalf("%s completed audit rows=%d, want operation/payment/event plus duplicate event", tc.driver, got)
			}
			if got := query("SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", siteName); got != 4 {
				t.Fatalf("%s outbox rows=%d, want operation/payment/event plus duplicate event", tc.driver, got)
			}

			// Exercise duplicate delivery from the same pending state on each
			// dialect. Both requests start together; only one may transition the
			// operation/payment, while the other must be acknowledged as a retry.
			concurrentSuffix := suffix + "-race"
			seed("INSERT INTO "+dialect.QuoteIdent(operation.RawTableName())+" (name, operation_type, purpose, source_doctype, provider, status, provider_request_id) VALUES (?, ?, ?, ?, ?, ?, ?)", "OP-"+concurrentSuffix, "Payment", "POS payment", "Sale", "M-Pesa", "Pending", "checkout-"+concurrentSuffix)
			seed("INSERT INTO "+dialect.QuoteIdent(payment.RawTableName())+" (name, external_operation, status) VALUES (?, ?, ?)", "PAY-"+concurrentSuffix, "OP-"+concurrentSuffix, "Pending")
			concurrentBody := `{"Body":{"stkCallback":{"CheckoutRequestID":"checkout-` + concurrentSuffix + `","ResultCode":0,"ResultDesc":"Success","CallbackMetadata":{"Item":[{"Name":"MpesaReceiptNumber","Value":"RCP-` + concurrentSuffix + `"}]}}}}`
			start := make(chan struct{})
			type callbackResult struct {
				status int
				body   string
			}
			results := make(chan callbackResult, 2)
			for range 2 {
				go func() {
					<-start
					response := serveMPesaCallbackDialect(t, handler, database, registry, siteName, tc.driver, concurrentBody)
					results <- callbackResult{status: response.Code, body: response.Body.String()}
				}()
			}
			close(start)
			first, second := <-results, <-results
			for _, response := range []callbackResult{first, second} {
				if response.status != http.StatusOK {
					t.Fatalf("%s concurrent callback = %d: %s", tc.driver, response.status, response.body)
				}
			}
			statuses := first.body + second.body
			if !strings.Contains(statuses, `"payment_status":"Succeeded"`) || !strings.Contains(statuses, `"status":"duplicate"`) {
				t.Fatalf("%s concurrent callbacks must yield one success and one duplicate: %s / %s", tc.driver, first.body, second.body)
			}
			if got := query("SELECT COUNT(*) FROM "+dialect.QuoteIdent(operation.RawTableName())+" WHERE name = ? AND status = 'Succeeded' AND provider_reference = ?", "OP-"+concurrentSuffix, "RCP-"+concurrentSuffix); got != 1 {
				t.Fatalf("%s concurrent callback operation terminal state rows=%d, want one", tc.driver, got)
			}
			if got := query("SELECT COUNT(*) FROM "+dialect.QuoteIdent(payment.RawTableName())+" WHERE name = ? AND status = 'Succeeded' AND provider_reference = ?", "PAY-"+concurrentSuffix, "RCP-"+concurrentSuffix); got != 1 {
				t.Fatalf("%s concurrent callback payment terminal state rows=%d, want one", tc.driver, got)
			}
			if got := query("SELECT COUNT(*) FROM "+dialect.QuoteIdent(event.RawTableName())+" WHERE operation = ? AND event_type = 'Callback'", "OP-"+concurrentSuffix); got != 2 {
				t.Fatalf("%s concurrent callback evidence rows=%d, want processed plus duplicate", tc.driver, got)
			}
		})
	}
}

func installPaymentUpdateFailureTrigger(t *testing.T, database *sql.DB, dialect kdb.Dialect, payment *doctype.DocType, suffix string) func() {
	t.Helper()
	table := dialect.QuoteIdent(payment.RawTableName())
	trigger := dialect.QuoteIdent("kora_fail_payment_update_" + suffix)
	var drop func()
	switch dialect.DriverName() {
	case "postgres":
		function := dialect.QuoteIdent("kora_fail_payment_update_fn_" + suffix)
		if _, err := database.Exec(`CREATE FUNCTION ` + function + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced payment update failure'; END; $$`); err != nil {
			t.Fatalf("create PostgreSQL failure function: %v", err)
		}
		if _, err := database.Exec(`CREATE TRIGGER ` + trigger + ` BEFORE UPDATE ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION ` + function + `()`); err != nil {
			t.Fatalf("create PostgreSQL failure trigger: %v", err)
		}
		drop = func() {
			_, _ = database.Exec(`DROP TRIGGER IF EXISTS ` + trigger + ` ON ` + table)
			_, _ = database.Exec(`DROP FUNCTION IF EXISTS ` + function + `()`)
		}
	case "libsql":
		if _, err := database.Exec(`CREATE TRIGGER ` + trigger + ` BEFORE UPDATE ON ` + table + ` BEGIN SELECT RAISE(ABORT, 'forced payment update failure'); END`); err != nil {
			t.Fatalf("create LibSQL failure trigger: %v", err)
		}
		drop = func() { _, _ = database.Exec(`DROP TRIGGER IF EXISTS ` + trigger) }
	default:
		t.Fatalf("unsupported test driver for failure trigger: %s", dialect.DriverName())
	}
	t.Cleanup(drop)
	return drop
}

func serveMPesaCallbackDialect(t *testing.T, handler *Handler, database *sql.DB, registry *doctype.Registry, site, driver, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/payments/mpesa/callback", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("site_name", site)
	ctx.Set("site_db", database)
	ctx.Set("site_registry", registry)
	ctx.Set("site_db_type", driver)
	handler.HandleMPesaSTKCallback(ctx)
	return recorder
}
