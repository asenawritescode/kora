package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

func TestLivePostgresMPesaLookups(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_POSTGRES_DSN to test M-Pesa callback lookups against a disposable PostgreSQL database")
	}
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable PostgreSQL database:", err)
	}
	schemaName := fmt.Sprintf("kora_api_mpesa_%d", time.Now().UnixNano())
	if _, err := database.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		t.Fatal("create isolated PostgreSQL schema:", err)
	}
	defer func() { _, _ = database.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`) }()
	if _, err := database.Exec(`SET search_path TO "` + schemaName + `"`); err != nil {
		t.Fatal("select isolated PostgreSQL schema:", err)
	}
	if _, err := database.Exec(`CREATE TABLE "tabExternal Operation" (name TEXT PRIMARY KEY, provider_request_id TEXT, provider_reference TEXT)`); err != nil {
		t.Fatal("create external operation fixture:", err)
	}
	if _, err := database.Exec(`CREATE TABLE "tabPayment" (name TEXT PRIMARY KEY, external_operation TEXT)`); err != nil {
		t.Fatal("create payment fixture:", err)
	}
	if _, err := database.Exec(`INSERT INTO "tabExternal Operation" (name, provider_request_id, provider_reference) VALUES ($1,$2,$3)`, "op-1", "checkout-1", "receipt-1"); err != nil {
		t.Fatal("insert operation fixture:", err)
	}
	if _, err := database.Exec(`INSERT INTO "tabPayment" (name, external_operation) VALUES ($1,$2)`, "payment-1", "op-1"); err != nil {
		t.Fatal("insert payment fixture:", err)
	}

	registry := doctype.NewRegistry()
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: kdb.Resolve("postgres")})
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/payments/mpesa/callback", nil)
	ctx.Set("site_db", database)
	ctx.Set("site_registry", registry)
	ctx.Set("site_name", "site-pg")
	ctx.Set("site_db_type", "postgres")

	operation, err := externalOperationName(handler, ctx, database, "checkout-1")
	if err != nil || operation != "op-1" {
		t.Fatalf("external operation lookup = %q, %v; want op-1", operation, err)
	}
	payment, err := linkedPaymentName(handler, ctx, database, operation)
	if err != nil || payment != "payment-1" {
		t.Fatalf("linked payment lookup = %q, %v; want payment-1", payment, err)
	}
}
