//go:build integration

package api

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/schema"
	"github.com/asenawritescode/kora/secret"
	"github.com/gin-gonic/gin"
)

// TestDigiTaxWebhookUsesKernelAndPreservesCallbackContract is the cutover gate
// for DigiTax's current direct UPDATE of the eTIMS Invoice DocType.
func TestDigiTaxWebhookUsesKernelAndPreservesCallbackContract(t *testing.T) {
	database, site := newResourceIntegrationDB(t)
	dialect := kdb.Resolve("mysql")
	reg := doctype.NewRegistry()
	fields := []doctype.Field{
		{Fieldname: "sales_invoice", Fieldtype: "Data", Reqd: true},
		{Fieldname: "trader_invoice_number", Fieldtype: "Data", Unique: true, ReadOnly: true},
		{Fieldname: "provider", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "provider_status", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "status", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "provider_sale_id", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "etims_reference", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "invoice_number", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "receipt_number", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "serial_number", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "internal_data", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "receipt_signature", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "etims_url", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "sale_detail_url", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "callback_event", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "webhook_received_at", Fieldtype: "Datetime", ReadOnly: true},
		{Fieldname: "signed_at", Fieldtype: "Datetime", ReadOnly: true},
		{Fieldname: "customer_pin", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "tax_summary", Fieldtype: "JSON", ReadOnly: true},
		{Fieldname: "submission_response", Fieldtype: "JSON", ReadOnly: true},
	}
	dt := &doctype.DocType{Name: "eTIMS Invoice", Fields: fields}
	reg.LoadFull([]*doctype.DocType{dt}, []*doctype.Role{{Name: doctype.AdminRole}}, []*doctype.Permission{{
		Doctype: dt.Name, Role: doctype.AdminRole, Read: true, Write: true, Create: true, Delete: true,
	}})
	for _, ddl := range dialect.SystemTableSQL() {
		if strings.HasPrefix(strings.TrimSpace(ddl), "CREATE TABLE IF NOT EXISTS") {
			if _, err := database.Exec(ddl); err != nil {
				t.Fatalf("create system tables: %v", err)
			}
		}
	}
	for _, ddl := range append(kdb.OutboxTablesMySQL(), kdb.KernelTablesMySQL()...) {
		if _, err := database.Exec(ddl); err != nil {
			t.Fatalf("create kernel/outbox tables: %v", err)
		}
	}
	if err := schema.MigrateSiteFromRegistry(database, site, reg, dialect); err != nil {
		t.Fatalf("migrate site schema: %v", err)
	}
	if _, err := database.Exec("INSERT INTO `tabeTIMS Invoice` (name, sales_invoice, trader_invoice_number) VALUES (?, ?, ?)", "ETIMS-0001", "SINV-0001", "INV-1"); err != nil {
		t.Fatalf("seed eTIMS invoice: %v", err)
	}
	if err := secret.NewStore(database, dialect).Set(site, "digitax_webhook_secret", "callback-secret"); err != nil {
		t.Fatalf("configure webhook secret: %v", err)
	}
	writer := outbox.NewSQLWriter(dialect)
	handler := NewHandler(reg, &orm.TxManager{DB: database, Registry: reg, Dialect: dialect, Outbox: writer, SiteName: site})
	handler.SiteOutboxes = map[string]outbox.Writer{site: writer}
	t.Cleanup(func() { _ = database.Close() })

	body := `{"event":"sale.sync","data":{"trader_invoice_number":"INV-1","queue_status":"completed","digitax_id":"DT-123","serial_number":"SER-1","invoice_number":"INV-100","receipt_number":"RCPT-1","receipt_signature":"sig","internal_data":"data","etims_url":"https://kra.example/verify","sale_detail_url":"https://digitax.example/sale","date":"29/09/2026","time":"12:00:00 pm","customer_pin":"A123","sales_tax_summary":{"vat":100}}}`
	first := serveDigiTaxWebhook(t, handler, database, reg, site, body, "callback-secret")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"status":"received"`) {
		t.Fatalf("callback response = HTTP %d %s", first.Code, first.Body.String())
	}
	var providerStatus, status, receipt string
	if err := database.QueryRow("SELECT provider_status, status, receipt_number FROM `tabeTIMS Invoice` WHERE name = ?", "ETIMS-0001").Scan(&providerStatus, &status, &receipt); err != nil {
		t.Fatal("read updated invoice:", err)
	}
	if providerStatus != "completed" || status != "Accepted" || receipt != "RCPT-1" {
		t.Fatalf("callback did not preserve invoice update: provider=%q status=%q receipt=%q", providerStatus, status, receipt)
	}
	var audits, events int
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doc_name = ? AND status = 'completed'", site, "ETIMS-0001").Scan(&audits); err != nil {
		t.Fatal("count kernel audit:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ?", site, "ETIMS-0001").Scan(&events); err != nil {
		t.Fatal("count outbox events:", err)
	}
	if audits != 1 || events != 1 {
		t.Fatalf("callback effects audit=%d outbox=%d, want 1 each", audits, events)
	}

	retry := serveDigiTaxWebhook(t, handler, database, reg, site, body, "callback-secret")
	if retry.Code != http.StatusOK || retry.Header().Get("X-Kora-Replay") != "true" {
		var stored sql.NullString
		_ = database.QueryRow("SELECT submission_response FROM `tabeTIMS Invoice` WHERE name = ?", "ETIMS-0001").Scan(&stored)
		t.Fatalf("identical callback retry = HTTP %d replay=%q stored=%q %s", retry.Code, retry.Header().Get("X-Kora-Replay"), stored.String, retry.Body.String())
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doc_name = ? AND status = 'completed'", site, "ETIMS-0001").Scan(&audits); err != nil {
		t.Fatal("recount kernel audit:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ?", site, "ETIMS-0001").Scan(&events); err != nil {
		t.Fatal("recount outbox:", err)
	}
	if audits != 1 || events != 1 {
		t.Fatalf("identical retry duplicated effects audit=%d outbox=%d", audits, events)
	}

	badSecret := serveDigiTaxWebhook(t, handler, database, reg, site, body, "wrong-secret")
	if badSecret.Code != http.StatusUnauthorized {
		t.Fatalf("invalid secret status = %d, want 401: %s", badSecret.Code, badSecret.Body.String())
	}
	malformed := serveDigiTaxWebhook(t, handler, database, reg, site, "{", "callback-secret")
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed callback status = %d, want 400: %s", malformed.Code, malformed.Body.String())
	}
	ignored := serveDigiTaxWebhook(t, handler, database, reg, site, `{"event":"ping","data":{}}`, "callback-secret")
	if ignored.Code != http.StatusOK || !strings.Contains(ignored.Body.String(), `"status":"ignored"`) {
		t.Fatalf("unsupported callback event = HTTP %d %s", ignored.Code, ignored.Body.String())
	}
	missing := serveDigiTaxWebhook(t, handler, database, reg, site,
		strings.Replace(body, `"INV-1"`, `"INV-MISSING"`, 1), "callback-secret")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing invoice status = %d, want 404: %s", missing.Code, missing.Body.String())
	}

	if _, err := database.Exec(`CREATE TRIGGER reject_digitax_update BEFORE UPDATE ON ` + "`tabeTIMS Invoice`" + ` FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'intentional test failure'`); err != nil {
		t.Fatalf("install update failure trigger: %v", err)
	}
	failedEvent := strings.Replace(body, `"queue_status":"completed"`, `"queue_status":"failed"`, 1)
	failed := serveDigiTaxWebhook(t, handler, database, reg, site, failedEvent, "callback-secret")
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("persistence failure status = %d, want 500: %s", failed.Code, failed.Body.String())
	}
	if _, err := database.Exec("DROP TRIGGER reject_digitax_update"); err != nil {
		t.Fatalf("remove update failure trigger: %v", err)
	}
	if err := database.QueryRow("SELECT status FROM `tabeTIMS Invoice` WHERE name = ?", "ETIMS-0001").Scan(&status); err != nil {
		t.Fatal("read invoice after failed callback:", err)
	}
	if status != "Accepted" {
		t.Fatalf("failed callback partially changed invoice status to %q", status)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doc_name = ? AND status = 'completed'", site, "ETIMS-0001").Scan(&audits); err != nil {
		t.Fatal("recount completed kernel audit:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ?", site, "ETIMS-0001").Scan(&events); err != nil {
		t.Fatal("recount outbox after rollback:", err)
	}
	if audits != 1 || events != 1 {
		t.Fatalf("failed callback left committed effects audit=%d outbox=%d", audits, events)
	}
}

func serveDigiTaxWebhook(t *testing.T, handler *Handler, database *sql.DB, reg *doctype.Registry, site, body, providedSecret string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/webhooks/digitax/etims", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set("X-DigiTax-Webhook-Secret", providedSecret)
	ctx.Set("site_db", database)
	ctx.Set("site_name", site)
	ctx.Set("site_registry", reg)
	ctx.Set("site_db_type", "mysql")
	handler.HandleDigiTaxWebhook(ctx)
	return recorder
}
