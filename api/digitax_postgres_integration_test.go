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
	"github.com/asenawritescode/kora/secret"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

func TestLivePostgresDigiTaxWebhook(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_POSTGRES_DSN to test DigiTax webhooks against a disposable PostgreSQL database")
	}
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL database:", err)
	}
	database.SetMaxOpenConns(4)
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable PostgreSQL database:", err)
	}
	schemaName := fmt.Sprintf("kora_api_digitax_%d", time.Now().UnixNano())
	if _, err := database.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		t.Fatal("create isolated PostgreSQL schema:", err)
	}
	var originalSearchPath string
	if err := database.QueryRow(`SELECT quote_literal(current_setting('search_path'))`).Scan(&originalSearchPath); err != nil {
		t.Fatal("read original PostgreSQL search path:", err)
	}
	if _, err := database.Exec(`ALTER ROLE CURRENT_USER SET search_path TO "` + schemaName + `"`); err != nil {
		t.Fatal("configure isolated PostgreSQL search path:", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal("close setup PostgreSQL pool:", err)
	}
	database, err = sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("reopen isolated PostgreSQL pool:", err)
	}
	database.SetMaxOpenConns(4)
	defer func() {
		cleanupDB, openErr := sql.Open("postgres", dsn)
		if openErr == nil {
			_, _ = cleanupDB.Exec(`ALTER ROLE CURRENT_USER SET search_path TO ` + originalSearchPath)
			_, _ = cleanupDB.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`)
			_ = cleanupDB.Close()
		}
		_ = database.Close()
	}()
	if err := database.Ping(); err != nil {
		t.Fatal("reopened PostgreSQL pool:", err)
	}
	dialect := kdb.Resolve("postgres")
	fields := []doctype.Field{
		{Fieldname: "sales_invoice", Fieldtype: "Data", Reqd: true},
		{Fieldname: "trader_invoice_number", Fieldtype: "Data", Unique: true, ReadOnly: true},
		{Fieldname: "provider", Fieldtype: "Data", ReadOnly: true}, {Fieldname: "provider_status", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "status", Fieldtype: "Data", ReadOnly: true}, {Fieldname: "provider_sale_id", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "etims_reference", Fieldtype: "Data", ReadOnly: true}, {Fieldname: "invoice_number", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "receipt_number", Fieldtype: "Data", ReadOnly: true}, {Fieldname: "serial_number", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "internal_data", Fieldtype: "Data", ReadOnly: true}, {Fieldname: "receipt_signature", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "etims_url", Fieldtype: "Data", ReadOnly: true}, {Fieldname: "sale_detail_url", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "callback_event", Fieldtype: "Data", ReadOnly: true}, {Fieldname: "webhook_received_at", Fieldtype: "Datetime", ReadOnly: true},
		{Fieldname: "signed_at", Fieldtype: "Datetime", ReadOnly: true}, {Fieldname: "customer_pin", Fieldtype: "Data", ReadOnly: true},
		{Fieldname: "tax_summary", Fieldtype: "JSON", ReadOnly: true}, {Fieldname: "submission_response", Fieldtype: "JSON", ReadOnly: true},
	}
	dt := &doctype.DocType{Name: "eTIMS Invoice", Fields: fields}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{dt}, []*doctype.Role{{Name: doctype.AdminRole}}, []*doctype.Permission{{
		Doctype: dt.Name, Role: doctype.AdminRole, Read: true, Write: true, Create: true, Delete: true,
	}})
	for _, ddl := range dialect.SystemTableSQL() {
		if strings.HasPrefix(strings.TrimSpace(ddl), "CREATE TABLE IF NOT EXISTS") {
			if _, err := database.Exec(ddl); err != nil {
				t.Fatal("create system table:", err)
			}
		}
	}
	for _, ddl := range append(kdb.OutboxTablesPostgres(), kdb.KernelTablesPostgres()...) {
		if _, err := database.Exec(ddl); err != nil {
			t.Fatal("create kernel/outbox table:", err)
		}
	}
	if err := schema.MigrateSiteFromRegistry(database, "site-pg", registry, dialect); err != nil {
		t.Fatal("migrate eTIMS invoice schema:", err)
	}
	if _, err := database.Exec(`INSERT INTO "tabeTIMS Invoice" (name, sales_invoice, trader_invoice_number) VALUES ($1, $2, $3)`, "ETIMS-PG-1", "SINV-PG-1", "INV-1"); err != nil {
		t.Fatal("insert eTIMS invoice fixture:", err)
	}
	store := secret.NewStore(database, dialect)
	if err := store.Set("site-pg", "digitax_webhook_secret", "test-webhook-secret"); err != nil {
		t.Fatal("store test webhook secret:", err)
	}
	writer := outbox.NewSQLWriter(dialect)
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: dialect, Outbox: writer, SiteName: "site-pg"})
	handler.SiteOutboxes = map[string]outbox.Writer{"site-pg": writer}
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/webhooks/digitax", strings.NewReader(`{"event":"sale.sync","data":{"trader_invoice_number":"INV-1","queue_status":"completed","digitax_id":"DT-123","serial_number":"SER-1","invoice_number":"INV-100","receipt_number":"RCPT-1","receipt_signature":"sig","internal_data":"data","etims_url":"https://kra.example/verify","sale_detail_url":"https://digitax.example/sale","date":"29/09/2026","time":"12:00:00 pm","customer_pin":"A123","sales_tax_summary":{"vat":100}}}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set("X-DigiTax-Webhook-Secret", "test-webhook-secret")
	ctx.Set("site_db", database)
	ctx.Set("site_name", "site-pg")
	ctx.Set("site_db_type", "postgres")
	handler.HandleDigiTaxWebhook(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("live PostgreSQL DigiTax webhook returned HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	var providerStatus, status, receipt, event string
	var taxSummary string
	if err := database.QueryRow(`SELECT provider_status, status, receipt_number, callback_event, tax_summary::text FROM "tabeTIMS Invoice" WHERE trader_invoice_number = $1`, "INV-1").Scan(&providerStatus, &status, &receipt, &event, &taxSummary); err != nil {
		t.Fatal("read updated eTIMS invoice:", err)
	}
	if providerStatus != "completed" || status != "Accepted" || receipt != "RCPT-1" || event != "sale.sync" || taxSummary == "" {
		t.Fatalf("unexpected eTIMS callback state: provider=%q status=%q receipt=%q event=%q tax=%q", providerStatus, status, receipt, event, taxSummary)
	}
	var audits, events int
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_operation_audit WHERE site = $1 AND doc_name = $2 AND status = 'completed'`, "site-pg", "ETIMS-PG-1").Scan(&audits); err != nil {
		t.Fatal("count kernel audit:", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_outbox WHERE site = $1 AND aggregate_id = $2`, "site-pg", "ETIMS-PG-1").Scan(&events); err != nil {
		t.Fatal("count outbox event:", err)
	}
	if audits != 1 || events != 1 {
		t.Fatalf("callback kernel side effects audit=%d outbox=%d", audits, events)
	}
}
