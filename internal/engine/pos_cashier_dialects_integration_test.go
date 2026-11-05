//go:build integration

package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/api"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/payment"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/site"
	"github.com/asenawritescode/kora/storage"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// Acceptance test (D7): a Cashier runs a whole POS day through published
// operations and queries alone, and cannot read cost or write POS/stock
// records through the generic routes.
func TestPOSCashierDayAcrossDialects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_CONFIG_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_CONFIG_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable database", tc.env)
			}
			database, dbName := isolatedDB(t, tc.driver, dsn)
			dialect := db.Resolve(tc.driver)
			if err := site.BootstrapSystemTables(database, dialect); err != nil {
				t.Fatal(err)
			}
			siteName := fmt.Sprintf("pos-cashier-%d.example.test", time.Now().UnixNano())
			if err := site.ImportConfig(database, doctype.NewRegistry(), dbName, siteName, filepath.Join("..", "..", "config", "pos"), dialect); err != nil {
				t.Fatalf("import: %v", err)
			}
			info := site.DBSiteInfo{SiteID: siteName, Name: siteName, DBType: tc.driver, DBName: dbName, Status: "active"}
			common := &site.CommonConfig{DBType: tc.driver}
			loaded, err := loadTenantRuntime(context.Background(), info, common, nil, "test", tenantRuntimeDependencies{
				connect:   func(*site.SiteConfig) (*sql.DB, error) { return database, nil },
				bootstrap: func(*sql.DB, db.Dialect) error { return nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			install, err := NewTenantServiceInstaller(TenantServiceConfig{
				Common: common, KernelCommands: kernel.NewCommandRegistry(),
				ResolveStorage: func(*site.SiteConfig) (storage.Backend, error) {
					return storage.New(storage.Config{Backend: "local", LocalPath: t.TempDir()})
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := install(info, loaded); err != nil {
				t.Fatal(err)
			}
			services := *loaded.RuntimeServices
			handler := api.NewHandler(loaded.Registry, nil)

			exec := func(role string) policy.Execution {
				e, err := policy.NewVerifiedExecution(contract.TenantID(siteName), contract.ActorContext{
					PrincipalID: role, SubjectUserID: role, PrincipalType: contract.PrincipalHuman, Site: siteName,
					Roles: []string{role}, AuthenticatedAt: time.Now().UTC(),
				})
				if err != nil {
					t.Fatal(err)
				}
				return e
			}
			// One router; the acting role is switched per request.
			var acting = "Cashier"
			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Set("site_name", siteName)
				c.Set("site_db", database)
				c.Set("site_db_type", tc.driver)
				c.Set("site_registry", loaded.Registry)
				c.Set("site_runtime_services", services)
				c.Set("policy_execution", exec(acting))
			})
			router.GET("/catalog", handler.HandleApplicationCatalog)
			router.POST("/op/:operation", handler.HandleExecutePublishedOperation)
			router.POST("/q/:module/:query", handler.HandlePublishedModuleQuery)
			router.GET("/r/:doctype", handler.HandleList)
			router.POST("/r/:doctype", handler.HandleCreate)
			router.GET("/r/:doctype/:name", handler.HandleGet)
			router.PUT("/r/:doctype/:name", handler.HandleUpdate)
			router.DELETE("/r/:doctype/:name", handler.HandleDelete)
			router.POST("/kernel/:command", handler.HandleKernelOperation)
			serve := func(role, method, path, body string) *httptest.ResponseRecorder {
				acting = role
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
				return rec
			}

			tbl := func(n string) string { return dialect.QuoteIdent(loaded.Registry.Get(n).RawTableName()) }
			one := func(q string, args ...any) (v string) {
				t.Helper()
				if err := database.QueryRow(db.Rebind(dialect, q), args...).Scan(&v); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				return v
			}
			count := func(doc string) string { return one("SELECT COUNT(*) FROM " + tbl(doc)) }
			qty := func() string {
				return one("SELECT quantity FROM "+tbl("Stock Balance")+" WHERE item = ?", "ITM-1")
			}
			// Administrator seed.
			seed := func(doc, name string, cols map[string]any) {
				t.Helper()
				keys, marks, args := []string{dialect.QuoteIdent("name")}, []string{"?"}, []any{name}
				for k, v := range cols {
					keys, marks, args = append(keys, dialect.QuoteIdent(k)), append(marks, "?"), append(args, v)
				}
				q := "INSERT INTO " + tbl(doc) + " (" + strings.Join(keys, ",") + ") VALUES (" + strings.Join(marks, ",") + ")"
				if _, err := database.Exec(db.Rebind(dialect, q), args...); err != nil {
					t.Fatalf("seed %s: %v", doc, err)
				}
			}
			seed("Location", "LOC-1", map[string]any{"location_name": "Main"})
			seed("Register", "REG-1", map[string]any{"register_name": "Front", "location": "LOC-1", "enabled": true})
			seed("Item", "ITM-1", map[string]any{"item_name": "Maize", "uom": "Nos", "price": 180, "cost": 120})
			seed("Stock Balance", "SB-1", map[string]any{"item": "ITM-1", "location": "LOC-1", "quantity": 20, "reorder_level": 4})
			payments := &payment.Service{Store: &payment.SQLStore{DB: database, Dialect: dialect}, Providers: payment.NewRegistry()}
			intent := func(key string, amt int64) string {
				in, err := payments.Request(context.Background(), siteName, "Cashier", payment.RequestInput{
					Provider: "cash", Amount: decimal.NewFromInt(amt), Currency: "KES", SourceDoctype: "Sale", IdempotencyKey: key,
				})
				if err != nil || in.State != payment.StateConfirmed {
					t.Fatalf("cash intent = %+v err=%v", in, err)
				}
				return in.ID
			}
			saleBody := func(till string, n int, ref string) string {
				return fmt.Sprintf(`{"register":"REG-1","location":"LOC-1","till_session":%q,"currency":"KES","items":[{"item":"ITM-1","quantity":%d,"unit_price":180,"amount":%d}],"payments":[{"method":"Cash","amount":%d,"currency":"KES","reference":%q}]}`, till, n, n*180, n*180, ref)
			}

			// Cashier catalog -> operation refs and input schemas.
			rec := serve("Cashier", http.MethodGet, "/catalog", "")
			var cat struct {
				Data contract.ApplicationCatalog `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &cat); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("catalog %d %s", rec.Code, rec.Body.String())
			}
			run := func(name, key, input string) {
				t.Helper()
				for _, o := range cat.Data.PublishedOperations {
					if strings.HasSuffix(o.OperationRef, "~"+name) {
						rec := serve("Cashier", http.MethodPost, "/op/"+o.OperationRef, fmt.Sprintf(`{"meta":{"idempotency_key":%q},"input_schema":{"id":%q,"version":%d},"input":%s}`, key, o.InputSchema.ID, o.InputSchema.Version, input))
						var out struct {
							Data contract.OperationReceipt `json:"data"`
						}
						if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Data.Status != contract.ReceiptCompleted {
							t.Fatalf("%s status=%d body=%s", name, rec.Code, rec.Body.String())
						}
						return
					}
				}
				t.Fatalf("cashier catalog lacks %s", name)
			}

			// 1. open_shift, open_till.
			run("open_shift", "k-shift", `{"register":"REG-1"}`)
			shift := one("SELECT name FROM " + tbl("Shift"))
			run("open_till", "k-till", fmt.Sprintf(`{"shift":%q,"opening_float":500}`, shift))
			till := one("SELECT name FROM " + tbl("Till Session"))

			// 2. item_search by term, no cost key.
			rec = serve("Cashier", http.MethodPost, "/q/inventory.core/item_search", `{"input":{"term":"Maize"}}`)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Maize") || strings.Contains(rec.Body.String(), `"cost"`) {
				t.Fatalf("item_search status=%d body=%s", rec.Code, rec.Body.String())
			}

			// 3. sale, return, void, close_till.
			run("record_sale", "k-sale1", saleBody(till, 2, intent("pay-1", 360)))
			if got := qty(); !strings.HasPrefix(got, "18") {
				t.Fatalf("stock after sale = %s, want 18", got)
			}
			if got := one("SELECT COUNT(*) FROM "+tbl("Stock Entry")+" WHERE entry_type = ?", "Issue"); got != "1" {
				t.Fatalf("issue entries = %s, want 1", got)
			}
			sale1 := one("SELECT name FROM " + tbl("Sale"))
			run("return_sale", "k-ret", fmt.Sprintf(`{"register":"REG-1","location":"LOC-1","till_session":%q,"return_against":%q,"items":[{"item":"ITM-1","quantity":1,"unit_price":180,"amount":180}],"payments":[{"method":"Cash","amount":180,"currency":"KES","reference":"refund"}]}`, till, sale1))
			if got := qty(); !strings.HasPrefix(got, "19") {
				t.Fatalf("stock after return = %s, want 19", got)
			}
			// Invalid input (unknown top-level "currency") is a validation error
			// naming the field, never a 200 failed receipt; stock is untouched
			// and a replay of the same key returns the same error.
			badReturn := fmt.Sprintf(`{"register":"REG-1","location":"LOC-1","till_session":%q,"return_against":%q,"currency":"KES","items":[{"item":"ITM-1","quantity":1,"unit_price":180,"amount":180}],"payments":[{"method":"Cash","amount":180,"currency":"KES","reference":"refund"}]}`, till, sale1)
			salesBefore := count("Sale")
			for i := 0; i < 2; i++ {
				for _, o := range cat.Data.PublishedOperations {
					if !strings.HasSuffix(o.OperationRef, "~return_sale") {
						continue
					}
					rec := serve("Cashier", http.MethodPost, "/op/"+o.OperationRef, fmt.Sprintf(`{"meta":{"idempotency_key":"k-bad"},"input_schema":{"id":%q,"version":%d},"input":%s}`, o.InputSchema.ID, o.InputSchema.Version, badReturn))
					if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "currency") || !strings.Contains(rec.Body.String(), "VALIDATION") {
						t.Fatalf("invalid return_sale attempt %d status=%d body=%s", i, rec.Code, rec.Body.String())
					}
				}
			}
			if got := qty(); !strings.HasPrefix(got, "19") || count("Sale") != salesBefore {
				t.Fatalf("invalid return changed state: stock=%s sales=%s want %s", got, count("Sale"), salesBefore)
			}
			run("record_sale", "k-sale2", saleBody(till, 1, intent("pay-2", 180)))
			sale2 := one("SELECT name FROM "+tbl("Sale")+" WHERE return_against IS NULL AND name <> ?", sale1)
			run("void_sale", "k-void", fmt.Sprintf(`{"sale":%q,"shift":%q,"till_session":%q,"register":"REG-1","location":"LOC-1"}`, sale2, shift, till))
			if got := qty(); !strings.HasPrefix(got, "19") {
				t.Fatalf("stock after void = %s, want 19", got)
			}
			// expected_cash is an input of close_till today (no till status/expected-cash derivation yet, #233).
			run("close_till", "k-close", fmt.Sprintf(`{"shift":%q,"closing_count":860,"expected_cash":860}`, shift))

			// 4. Refusals: 403 and nothing written.
			sales, entries := count("Sale"), count("Stock Entry")
			deny := func(role, method, path, body string) {
				t.Helper()
				if rec := serve(role, method, path, body); rec.Code != http.StatusForbidden {
					t.Errorf("%s %s %s = %d, want 403: %s", role, method, path, rec.Code, rec.Body.String())
				}
			}
			deny("Cashier", http.MethodGet, "/r/Item/ITM-1", "")
			deny("Cashier", http.MethodGet, "/r/Item", "")
			deny("Cashier", http.MethodGet, "/r/Stock%20Balance/SB-1", "")
			deny("Cashier", http.MethodGet, "/r/Stock%20Balance", "")
			deny("Cashier", http.MethodPost, "/r/Sale", saleBody(till, 1, "x"))
			for _, role := range []string{"Cashier", "Owner"} {
				deny(role, http.MethodPut, "/r/Till%20Session/"+till, `{"expected_cash":1}`)
				deny(role, http.MethodDelete, "/r/Sale/"+sale1, "")
			}
			deny("Cashier", http.MethodPost, "/kernel/pos.record_sale", `{"idempotency_key":"k-direct","payload":`+saleBody(till, 1, "x")+`}`)
			if a, b := count("Sale"), count("Stock Entry"); a != sales || b != entries {
				t.Errorf("refused calls wrote data: sales %s->%s entries %s->%s", sales, a, entries, b)
			}
			if got := one("SELECT expected_cash FROM "+tbl("Till Session")+" WHERE name = ?", till); !strings.HasPrefix(got, "860") {
				t.Errorf("expected_cash changed to %s", got)
			}
			if got := one("SELECT COUNT(*) FROM "+tbl("Sale")+" WHERE name = ?", sale1); got != "1" {
				t.Error("sale deleted")
			}

			// 5. Cashier can read Sale and Till Session records.
			for _, p := range []string{"/r/Sale/" + sale1, "/r/Till%20Session/" + till} {
				if rec := serve("Cashier", http.MethodGet, p, ""); rec.Code != http.StatusOK {
					t.Errorf("GET %s = %d: %s", p, rec.Code, rec.Body.String())
				}
			}
		})
	}
}
