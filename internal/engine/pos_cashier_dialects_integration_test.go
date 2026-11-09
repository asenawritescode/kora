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
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/payment"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
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

			// actingUser, when set, is the verified subject while the role stays the
			// acting role: it models a second user holding the same role (#91).
			var actingUser string
			exec := func(role string) policy.Execution {
				subject := role
				if actingUser != "" {
					subject = actingUser
				}
				e, err := policy.NewVerifiedExecution(contract.TenantID(siteName), contract.ActorContext{
					PrincipalID: subject, SubjectUserID: subject, PrincipalType: contract.PrincipalHuman, Site: siteName,
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
			router.GET("/print/:format_name", api.NewSitePrintHandler(handler))
			router.GET("/print-formats", api.NewSitePrintListHandler(handler))
			router.GET("/print-formats/:format_name", api.NewSitePrintMetaHandler(handler))
			router.POST("/op/:operation", handler.HandleExecutePublishedOperation)
			router.POST("/q/:module/:query", handler.HandlePublishedModuleQuery)
			router.GET("/r/:doctype", handler.HandleList)
			router.POST("/r/:doctype", handler.HandleCreate)
			router.GET("/r/:doctype/:name", handler.HandleGet)
			router.PUT("/r/:doctype/:name", handler.HandleUpdate)
			router.DELETE("/r/:doctype/:name", handler.HandleDelete)
			router.POST("/kernel/:command", handler.HandleKernelOperation)
			router.POST("/payments", handler.HandlePaymentRequest)
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
			tryAs := func(role, name, key, input string) *httptest.ResponseRecorder {
				t.Helper()
				for _, o := range cat.Data.PublishedOperations {
					if strings.HasSuffix(o.OperationRef, "~"+name) {
						return serve(role, http.MethodPost, "/op/"+o.OperationRef, fmt.Sprintf(`{"meta":{"idempotency_key":%q},"input_schema":{"id":%q,"version":%d},"input":%s}`, key, o.InputSchema.ID, o.InputSchema.Version, input))
					}
				}
				t.Fatalf("catalog lacks %s", name)
				return nil
			}
			runAs := func(role, name, key, input string) string {
				t.Helper()
				rec := tryAs(role, name, key, input)
				var out struct {
					Data contract.OperationReceipt `json:"data"`
				}
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Data.Status != contract.ReceiptCompleted {
					t.Fatalf("%s as %s status=%d body=%s", name, role, rec.Code, rec.Body.String())
				}
				return string(out.Data.ExecutionID)
			}
			run := func(name, key, input string) string { t.Helper(); return runAs("Cashier", name, key, input) }

			// 0. #408: the catalog's open_session input lists every input the command
			// needs as required and typed, and a call without one names the field
			// instead of leaking the step index or $input syntax. Nothing is written.
			for _, o := range cat.Data.PublishedOperations {
				if !strings.HasSuffix(o.OperationRef, "~open_session") {
					continue
				}
				for _, sc := range cat.Data.Schemas {
					if sc.Ref != o.InputSchema {
						continue
					}
					var doc struct {
						Required   []string                  `json:"required"`
						Properties map[string]map[string]any `json:"properties"`
					}
					raw, _ := json.Marshal(sc.Schema)
					if json.Unmarshal(raw, &doc) != nil || strings.Join(doc.Required, ",") != "location,opening_float,register" ||
						doc.Properties["register"]["type"] != "string" || doc.Properties["location"]["type"] != "string" || doc.Properties["opening_float"]["type"] != "number" {
						t.Fatalf("open_session input schema = %s", raw)
					}
				}
			}
			for field, input := range map[string]string{"location": `{"register":"REG-1","opening_float":1000}`, "register": `{"location":"LOC-1","opening_float":1000}`} {
				rec := tryAs("Cashier", "open_session", "k-missing-"+field, input)
				body := rec.Body.String()
				if rec.Code != http.StatusBadRequest || !strings.Contains(body, `"input `+field+` is required"`) || !strings.Contains(body, `"field":"`+field+`"`) || strings.Contains(body, "$input") || strings.Contains(body, "require 1") {
					t.Fatalf("open_session without %s: status=%d body=%s", field, rec.Code, body)
				}
			}
			if got := count("Shift"); got != "0" {
				t.Fatalf("a refused open_session wrote %s shifts", got)
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

			// 2b. #406: Item Category is owned by inventory.core, so the Cashier's
			// query grant alone serves the register's category tabs, in the
			// catalog and through the route. Direct reads stay denied for the
			// Cashier; Manager and Owner keep their category read.
			seed("Item Category", "CAT-1", map[string]any{"category_name": "Grain", "display_order": 1, "enabled": true})
			seed("Item Category", "CAT-2", map[string]any{"category_name": "Retired", "display_order": 2, "enabled": false})
			for _, role := range []string{"Cashier", "Manager", "Owner"} {
				found := false
				for _, q := range cat.Data.PublishedQueries {
					found = found || q.Name == "item_categories" && q.OwnerModuleID == "inventory.core"
				}
				if !found {
					t.Fatalf("%s: catalog lacks item_categories", role)
				}
				rec := serve(role, http.MethodPost, "/q/inventory.core/item_categories", `{"input":{},"cursor":"","limit":5}`)
				var out struct {
					Data struct {
						Data []map[string]any `json:"data"`
					} `json:"data"`
				}
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Data.Data) != 1 || out.Data.Data[0]["category_name"] != "Grain" {
					t.Fatalf("item_categories as %s status=%d body=%s", role, rec.Code, rec.Body.String())
				}
			}
			if rec := serve("Cashier", http.MethodGet, "/r/Item%20Category", ""); rec.Code != http.StatusForbidden {
				t.Fatalf("Cashier direct Item Category list = %d, want 403: %s", rec.Code, rec.Body.String())
			}
			if rec := serve("Cashier", http.MethodPost, "/r/Item%20Category", `{"category_name":"X"}`); rec.Code != http.StatusForbidden {
				t.Fatalf("Cashier Item Category create = %d, want 403: %s", rec.Code, rec.Body.String())
			}

			// 2c. The Manager reads stock: item_stock and low_stock are served from
			// Stock Balance, which the Manager can read (read-only). The Cashier
			// holds neither query and stays denied, as on the record routes.
			for _, name := range []string{"item_stock", "low_stock"} {
				rec := serve("Manager", http.MethodPost, "/q/inventory.core/"+name, `{"input":{},"cursor":"","limit":5}`)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s as Manager status=%d, want 200: %s", name, rec.Code, rec.Body.String())
				}
				if rec := serve("Cashier", http.MethodPost, "/q/inventory.core/"+name, `{"input":{},"cursor":"","limit":5}`); rec.Code != http.StatusForbidden {
					t.Fatalf("%s as Cashier status=%d, want 403: %s", name, rec.Code, rec.Body.String())
				}
			}
			if rec := serve("Manager", http.MethodGet, "/r/Stock%20Balance/SB-1", ""); rec.Code != http.StatusOK {
				t.Fatalf("Manager Stock Balance read = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			for _, w := range []struct{ method, path, body string }{
				{http.MethodPost, "/r/Stock%20Balance", `{"item":"ITM-1","location":"LOC-1","quantity":1}`},
				{http.MethodPut, "/r/Stock%20Balance/SB-1", `{"quantity":999}`},
				{http.MethodDelete, "/r/Stock%20Balance/SB-1", ""},
			} {
				if rec := serve("Manager", w.method, w.path, w.body); rec.Code != http.StatusForbidden {
					t.Fatalf("Manager %s %s = %d, want 403: %s", w.method, w.path, rec.Code, rec.Body.String())
				}
			}
			if got, err := decimal.NewFromString(qty()); err != nil || !got.Equal(decimal.NewFromInt(20)) {
				t.Fatalf("Manager stock write leaked: quantity = %s (err %v), want 20", got, err)
			}

			// 3. #358: this till was opened the old way (no cashier recorded), so
			// the Cashier is refused on it; the Manager runs sale, return, void
			// and close_till.
			for name, input := range map[string]string{
				"record_sale": saleBody(till, 1, intent("pay-old", 180)),
				"close_till":  fmt.Sprintf(`{"shift":%q,"closing_count":1}`, shift),
			} {
				if rec := tryAs("Cashier", name, "k-old-"+name, input); rec.Code != http.StatusOK && rec.Code != http.StatusConflict || strings.Contains(rec.Body.String(), `"completed"`) || !strings.Contains(rec.Body.String(), "belongs to another cashier") {
					t.Fatalf("Cashier %s on an old-style till: status=%d body=%s", name, rec.Code, rec.Body.String())
				}
			}
			runAs("Manager", "record_sale", "k-sale1", saleBody(till, 2, intent("pay-1", 360)))
			if got := qty(); !strings.HasPrefix(got, "18") {
				t.Fatalf("stock after sale = %s, want 18", got)
			}
			if got := one("SELECT COUNT(*) FROM "+tbl("Stock Entry")+" WHERE entry_type = ?", "Issue"); got != "1" {
				t.Fatalf("issue entries = %s, want 1", got)
			}
			sale1 := one("SELECT name FROM " + tbl("Sale"))
			// #337: a Cashier (who cannot read Item) prints the sale and gets the
			// item name, not the record id.
			rec = serve("Cashier", http.MethodGet, "/print/sale_record_receipt?as=text&record=Sale&name="+sale1, "")
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Maize") || strings.Contains(rec.Body.String(), "ITM-1") {
				t.Fatalf("receipt print status=%d body=%s", rec.Code, rec.Body.String())
			}
			runAs("Manager", "return_sale", "k-ret", fmt.Sprintf(`{"register":"REG-1","location":"LOC-1","till_session":%q,"return_against":%q,"items":[{"item":"ITM-1","quantity":1,"unit_price":180,"amount":180}],"payments":[{"method":"Cash","amount":180,"currency":"KES","reference":"refund"}]}`, till, sale1))
			if got := qty(); !strings.HasPrefix(got, "19") {
				t.Fatalf("stock after return = %s, want 19", got)
			}
			// #329: a return starts from a receipt: sale_lines shows what is left
			// to return on the sale, for every till role, as exact decimals.
			for _, role := range []string{"Cashier", "Manager", "Owner"} {
				for sale, want := range map[string]int{sale1: 1, "SALE-404": 0} {
					rec := serve(role, http.MethodPost, "/q/pos.core/sale_lines", fmt.Sprintf(`{"input":{"sale":%q}}`, sale))
					var out struct {
						Data struct {
							Data []map[string]any `json:"data"`
						} `json:"data"`
					}
					if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Data.Data) != want {
						t.Fatalf("%s sale_lines %s status=%d body=%s", role, sale, rec.Code, rec.Body.String())
					}
					if want == 1 {
						r := out.Data.Data[0]
						if r["item"] != "ITM-1" || len(r) != 5 || !decimalIs(r["unit_price"], "180") || !decimalIs(r["sold_quantity"], "2") || !decimalIs(r["remaining_quantity"], "1") {
							t.Fatalf("%s sale_lines row = %v", role, r)
						}
					}
				}
				if rec := serve(role, http.MethodPost, "/q/pos.core/sale_lines", `{"input":{}}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "sale") {
					t.Fatalf("%s sale_lines without sale status=%d body=%s", role, rec.Code, rec.Body.String())
				}
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
			runAs("Manager", "record_sale", "k-sale2", saleBody(till, 1, intent("pay-2", 180)))
			sale2 := one("SELECT name FROM "+tbl("Sale")+" WHERE return_against IS NULL AND name <> ?", sale1)
			runAs("Manager", "void_sale", "k-void", fmt.Sprintf(`{"sale":%q,"shift":%q,"till_session":%q,"register":"REG-1","location":"LOC-1"}`, sale2, shift, till))
			if got := qty(); !strings.HasPrefix(got, "19") {
				t.Fatalf("stock after void = %s, want 19", got)
			}
			// expected_cash is server-derived (#233): 500 float + 360 - 180 + 180 - 180 = 680; close_till takes only the closing count. The Owner closes it (#396 prints it).
			ownerClose := runAs("Owner", "close_till", "k-close", fmt.Sprintf(`{"shift":%q,"closing_count":680}`, shift))

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
			if got := one("SELECT expected_cash FROM "+tbl("Till Session")+" WHERE name = ?", till); !strings.HasPrefix(got, "680") {
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

			// 6. #326: open_session opens shift + till in one operation and
			// my_session hands the register everything a sale needs.
			seed("Register", "REG-2", map[string]any{"register_name": "Side", "location": "LOC-1", "enabled": true})
			op := func(role, name, key, input string) *httptest.ResponseRecorder {
				for _, o := range cat.Data.PublishedOperations {
					if strings.HasSuffix(o.OperationRef, "~"+name) {
						return serve(role, http.MethodPost, "/op/"+o.OperationRef, fmt.Sprintf(`{"meta":{"idempotency_key":%q},"input_schema":{"id":%q,"version":%d},"input":%s}`, key, o.InputSchema.ID, o.InputSchema.Version, input))
					}
				}
				t.Fatalf("cashier catalog lacks %s", name)
				return nil
			}
			completed := func(rec *httptest.ResponseRecorder) bool {
				return rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), `"completed"`)
			}
			mySession := func(role string) []map[string]any {
				t.Helper()
				rec := serve(role, http.MethodPost, "/q/pos.core/my_session", `{"input":{}}`)
				var out struct {
					Data struct {
						Data []map[string]any `json:"data"`
					} `json:"data"`
				}
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
					t.Fatalf("%s my_session status=%d body=%s", role, rec.Code, rec.Body.String())
				}
				return out.Data.Data
			}
			if rows := mySession("Cashier"); len(rows) != 0 {
				t.Fatalf("my_session before open = %v", rows)
			}
			// #374: my_registers lists the enabled registers for every till role,
			// ordered by name, without any generic Register read route.
			seed("Register", "REG-OFF", map[string]any{"register_name": "Aaa retired", "location": "LOC-1", "enabled": false})
			for _, role := range []string{"Cashier", "Manager", "Owner"} {
				rec := serve(role, http.MethodPost, "/q/pos.core/my_registers", `{"input":{}}`)
				var out struct {
					Data struct {
						Data []map[string]any `json:"data"`
					} `json:"data"`
				}
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Data.Data) != 2 {
					t.Fatalf("%s my_registers status=%d body=%s", role, rec.Code, rec.Body.String())
				}
				if r := out.Data.Data; r[0]["name"] != "REG-1" || r[0]["register_name"] != "Front" || r[0]["location"] != "LOC-1" || r[1]["name"] != "REG-2" || r[1]["register_name"] != "Side" || len(r[0]) != 3 {
					t.Fatalf("%s my_registers rows = %v", role, r)
				}
			}
			if rec := op("Cashier", "open_session", "k-sess-bad", `{"register":"REG-2","location":"LOC-X","opening_float":300}`); completed(rec) {
				t.Fatalf("open_session with a foreign location succeeded: %s", rec.Body.String())
			}
			run("open_session", "k-sess", `{"register":"REG-2","location":"LOC-1","opening_float":300}`)
			rows := mySession("Cashier")
			if len(rows) != 1 || rows[0]["register"] != "REG-2" || rows[0]["location"] != "LOC-1" || rows[0]["cashier"] != "Cashier" || rows[0]["shift"] == nil || !strings.HasPrefix(fmt.Sprint(rows[0]["opening_float"]), "300") {
				t.Fatalf("my_session = %v", rows)
			}
			session := fmt.Sprint(rows[0]["name"])
			if got := one("SELECT cashier FROM "+tbl("Shift")+" WHERE name = ?", fmt.Sprint(rows[0]["shift"])); got != "Cashier" {
				t.Fatalf("session shift cashier = %q", got)
			}
			if rows := mySession("Manager"); len(rows) != 0 {
				t.Fatalf("another actor sees the cashier's session: %v", rows)
			}
			for _, role := range []string{"Cashier", "Manager"} {
				if rec := op(role, "open_session", "k-sess-2-"+role, `{"register":"REG-2","location":"LOC-1","opening_float":10}`); completed(rec) {
					t.Fatalf("%s second open_session on REG-2 succeeded: %s", role, rec.Body.String())
				}
			}
			if got := one("SELECT COUNT(*) FROM "+tbl("Till Session")+" WHERE status = ?", "Open"); got != "1" {
				t.Fatalf("open tills = %s, want 1", got)
			}
			// #319: the Cashier creates the intent itself through POST /payments,
			// authorized by the record_sale operation grant, not by Sale create.
			var saleRef string
			for _, o := range cat.Data.PublishedOperations {
				if strings.HasSuffix(o.OperationRef, "~record_sale") {
					saleRef = o.OperationRef
					if o.Payment == nil || o.Payment.SourceDoctype != "Sale" {
						t.Fatalf("record_sale payment descriptor = %+v", o.Payment)
					}
				}
			}
			postIntent := func(role, key, operationRef string) *httptest.ResponseRecorder {
				acting = role
				ref := ""
				if operationRef != "" {
					ref = fmt.Sprintf(`,"operation_ref":%q`, operationRef)
				}
				req := httptest.NewRequest(http.MethodPost, "/payments", strings.NewReader(`{"provider":"cash","amount":"180","currency":"KES","source_doctype":"Sale"`+ref+`}`))
				req.Header.Set("Idempotency-Key", key)
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				return rec
			}
			if rec := postIntent("Cashier", "pay-noop", ""); rec.Code != http.StatusForbidden {
				t.Fatalf("cashier intent without operation_ref = %d, want 403: %s", rec.Code, rec.Body.String())
			}
			if rec := postIntent("Cashier", "pay-badop", saleRef+"x"); rec.Code != http.StatusNotFound {
				t.Fatalf("cashier intent with unknown operation = %d, want 404: %s", rec.Code, rec.Body.String())
			}
			rec = postIntent("Cashier", "pay-sess", saleRef)
			var created struct {
				Data struct {
					ID    string `json:"id"`
					State string `json:"state"`
				} `json:"data"`
			}
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &created) != nil || created.Data.State != "confirmed" {
				t.Fatalf("cashier intent = %d %s", rec.Code, rec.Body.String())
			}
			body := fmt.Sprintf(`{"register":"REG-2","location":"LOC-1","till_session":%q,"currency":"KES","items":[{"item":"ITM-1","quantity":1,"unit_price":180,"amount":180}],"payments":[{"method":"Cash","amount":180,"currency":"KES","reference":%q}]}`, session, created.Data.ID)
			run("record_sale", "k-sess-sale", body)
			if got := one("SELECT location FROM "+tbl("Sale")+" WHERE till_session = ?", session); got != "LOC-1" {
				t.Fatalf("sale location = %q", got)
			}

			// 7. #384: closed_session reads a closed till by reference with the
			// exact cash figures, and only the right actors see it.
			// The catalog shows each role one of the two: Cashier my_closed_session, Manager/Owner closed_session.
			closedQuery := func(role string) string {
				if role == "Cashier" {
					return "my_closed_session"
				}
				return "closed_session"
			}
			closedSession := func(role, name string) []map[string]any {
				t.Helper()
				rec := serve(role, http.MethodPost, "/q/pos.core/"+closedQuery(role), fmt.Sprintf(`{"input":{"name":%q}}`, name))
				var out struct {
					Data struct {
						Data []map[string]any `json:"data"`
					} `json:"data"`
				}
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
					t.Fatalf("%s %s status=%d body=%s", role, closedQuery(role), rec.Code, rec.Body.String())
				}
				return out.Data.Data
			}
			if rows := closedSession("Cashier", session); len(rows) != 0 {
				t.Fatalf("closed_session returned an open till: %v", rows)
			}
			// 300 float + 180 sale = 480 expected; the cashier counts 475.
			cashierClose := run("close_till", "k-close-sess", fmt.Sprintf(`{"shift":%q,"closing_count":475}`, rows[0]["shift"]))
			for _, tc := range []struct {
				role, name string
				want       int
			}{{"Cashier", session, 1}, {"Manager", session, 1}, {"Owner", session, 1}, {"Cashier", till, 0}, {"Manager", till, 1}, {"Owner", till, 1}} {
				got := closedSession(tc.role, tc.name)
				if len(got) != tc.want {
					t.Fatalf("%s closed_session %s = %v, want %d row(s)", tc.role, tc.name, got, tc.want)
				}
				if tc.want == 1 && tc.name == session {
					// Decimals are exact strings, variance = closing_count - expected_cash.
					r := got[0]
					exact := func(field, want string) bool {
						text, isText := r[field].(string)
						d, err := decimal.NewFromString(text)
						return isText && err == nil && d.Equal(decimal.RequireFromString(want))
					}
					if r["name"] != session || r["cashier"] != "Cashier" || !exact("opening_float", "300") || !exact("expected_cash", "480") || !exact("closing_count", "475") || !exact("variance", "-5") || r["closed_at"] == nil || len(r) != 10 {
						t.Fatalf("%s closed_session row = %#v", tc.role, r)
					}
				}
			}
			for _, role := range []string{"Cashier", "Manager", "Owner"} {
				if rec := serve(role, http.MethodPost, "/q/pos.core/"+closedQuery(role), `{"input":{}}`); rec.Code != http.StatusBadRequest {
					t.Fatalf("%s %s without name = %d: %s", role, closedQuery(role), rec.Code, rec.Body.String())
				}
			}
			// No role holds both queries.
			for role, other := range map[string]string{"Cashier": "closed_session", "Manager": "my_closed_session", "Owner": "my_closed_session"} {
				if rec := serve(role, http.MethodPost, "/q/pos.core/"+other, fmt.Sprintf(`{"input":{"name":%q}}`, session)); rec.Code != http.StatusForbidden {
					t.Fatalf("%s %s = %d, want 403: %s", role, other, rec.Code, rec.Body.String())
				}
			}

			// 8. #390: close_session closes the till and its shift in one
			// operation. close_till above left the shift open, so close it,
			// open a fresh session, and close that one with close_session.
			run("close_shift", "k-close-shift", `{"register":"REG-2"}`)
			run("open_session", "k-sess-again", `{"register":"REG-2","location":"LOC-1","opening_float":20}`)
			rows = mySession("Cashier")
			if len(rows) != 1 || rows[0]["name"] == session {
				t.Fatalf("my_session after reopen = %v", rows)
			}
			second, secondShift := fmt.Sprint(rows[0]["name"]), fmt.Sprint(rows[0]["shift"])
			if rec := op("Cashier", "close_session", "k-close-nobody", `{"shift":"SHF-none","closing_count":1}`); completed(rec) {
				t.Fatalf("close_session on an unknown shift succeeded: %s", rec.Body.String())
			}
			sessionClose := run("close_session", "k-close-session", fmt.Sprintf(`{"shift":%q,"closing_count":25}`, secondShift))
			if got := one("SELECT status FROM "+tbl("Till Session")+" WHERE name = ?", second) + "/" + one("SELECT status FROM "+tbl("Shift")+" WHERE name = ?", secondShift); got != "Closed/Closed" {
				t.Fatalf("till/shift after close_session = %s", got)
			}
			if got := one("SELECT variance FROM "+tbl("Till Session")+" WHERE name = ?", second); !decimalIs(got, "5") {
				t.Fatalf("variance = %s, want 5", got)
			}
			if rec := op("Cashier", "close_session", "k-close-again", fmt.Sprintf(`{"shift":%q,"closing_count":1}`, secondShift)); completed(rec) {
				t.Fatalf("second close_session succeeded: %s", rec.Body.String())
			}

			// An Owner till for the close_session print: close the shift
			// close_till left open on REG-1, open a session there, close it
			// counting 10 over.
			runAs("Owner", "close_shift", "k-owner-shift", `{"register":"REG-1"}`)
			runAs("Owner", "open_session", "k-owner-sess", `{"register":"REG-1","location":"LOC-1","opening_float":40}`)
			ownerTill := one("SELECT name FROM "+tbl("Till Session")+" WHERE register = ? AND status = ?", "REG-1", "Open")
			ownerShift := one("SELECT shift FROM "+tbl("Till Session")+" WHERE name = ?", ownerTill)
			ownerSessionClose := runAs("Owner", "close_session", "k-owner-session", fmt.Sprintf(`{"shift":%q,"closing_count":50}`, ownerShift))

			// 9. #396: the Z report prints for an execution of close_session as
			// well as close_till, with the same exact figures, for the Owner; a
			// Cashier is refused (policy).
			opRef := func(name string) string {
				t.Helper()
				for _, o := range cat.Data.PublishedOperations {
					if strings.HasSuffix(o.OperationRef, "~"+name) {
						return o.OperationRef
					}
				}
				t.Fatalf("catalog lacks %s", name)
				return ""
			}
			// Text lines are compared whitespace-collapsed; html fragments are exact.
			for _, tc := range []struct {
				name, role, format, op, exec string
				status                       int
				text, html                   []string
			}{
				{"owner close_till own till", "Owner", "z_report", "close_till", ownerClose, 200,
					[]string{"Z REPORT", "Till: " + till, "SALES Cash 1 360.00 REFUNDS Cash 1 180.00", "Opening float 500.00 Expected cash 680.00 Counted 680.00 VARIANCE 0.00"},
					[]string{"<h2>SALES</h2>", "<tr><td>Cash</td><td>1</td><td>360.00</td></tr>", "<h2>REFUNDS</h2>", "<tr><td>Cash</td><td>1</td><td>180.00</td></tr>", "<h2>Totals</h2>", `<th scope="row">Opening float</th><td>500.00</td>`, `<th scope="row">Expected cash</th><td>680.00</td>`, `<th scope="row">Counted cash</th><td>680.00</td>`, "<strong>0.00</strong>"}},
				{"owner close_session own till", "Owner", "z_report_close_session", "close_session", ownerSessionClose, 200,
					[]string{"Z REPORT", "Till: " + ownerTill, "SALES REFUNDS ", "Opening float 40.00 Expected cash 40.00 Counted 50.00 VARIANCE 10.00"},
					[]string{"<h2>SALES</h2>", "<h2>Totals</h2>", `<th scope="row">Opening float</th><td>40.00</td>`, `<th scope="row">Expected cash</th><td>40.00</td>`, `<th scope="row">Counted cash</th><td>50.00</td>`, "<strong>10.00</strong>"}},
				{"close_session execution under the close_till format", "Owner", "z_report", "close_till", sessionClose, 404, nil, nil},
				{"close_till format with the close_session ref", "Owner", "z_report", "close_session", ownerClose, 400, nil, nil},
				// Policy (#396): the per-method Z report is an Owner/Manager/Administrator
				// document. A Cashier holds no payments_by_method/refunds_by_method grant,
				// so the print is refused for both operations; they get the server
				// variance from my_closed_session instead.
				{"cashier close_till own till", "Cashier", "z_report", "close_till", cashierClose, 403, nil, nil},
				{"cashier close_session own till", "Cashier", "z_report_close_session", "close_session", sessionClose, 403, nil, nil},
				{"cashier on the owner's execution", "Cashier", "z_report", "close_till", ownerClose, 404, nil, nil},
			} {
				for _, as := range []string{"text", "html"} {
					rec := serve(tc.role, http.MethodGet, "/print/"+tc.format+"?as="+as+"&operation_ref="+opRef(tc.op)+"&execution_id="+tc.exec, "")
					if rec.Code != tc.status {
						t.Fatalf("%s as %s: status=%d want %d body=%s", tc.name, as, rec.Code, tc.status, rec.Body.String())
					}
					if tc.status == 403 && !strings.Contains(rec.Body.String(), "print.forbidden") {
						t.Fatalf("%s as %s: body=%s, want print.forbidden", tc.name, as, rec.Body.String())
					}
					body, want := rec.Body.String(), tc.html
					if as == "text" {
						body, want = strings.Join(strings.Fields(regexp.MustCompile(`-{5,}`).ReplaceAllString(body, "")), " "), tc.text
					}
					for _, w := range want {
						if !strings.Contains(body, w) {
							t.Errorf("%s as %s: missing %q in\n%s", tc.name, as, w, body)
						}
					}
					// A till with no refunds and no account rows prints no empty sections.
					if as == "html" && tc.name == "owner close_session own till" {
						for _, w := range []string{"REFUNDS", "BY ACCOUNT"} {
							if strings.Contains(body, w) {
								t.Errorf("%s as html: has %q in\n%s", tc.name, w, body)
							}
						}
					}
				}
			}
			// The metadata Studio's Print action reads lists only formats the actor
			// can print: a Cashier is not offered a Z report they would be refused.
			listed := func(role, query string) string {
				t.Helper()
				rec := serve(role, http.MethodGet, "/print-formats?"+query, "")
				if rec.Code != http.StatusOK {
					t.Fatalf("%s list %s = %d %s", role, query, rec.Code, rec.Body.String())
				}
				return rec.Body.String()
			}
			for _, tc := range []struct{ format, op string }{{"z_report", "close_till"}, {"z_report_close_session", "close_session"}} {
				if got := listed("Owner", "operation_ref="+opRef(tc.op)); !strings.Contains(got, `"name":"`+tc.format+`"`) || strings.Count(got, `"name":`) != 1 {
					t.Errorf("Owner list for %s = %s", tc.op, got)
				}
				if rec := serve("Owner", http.MethodGet, "/print-formats/"+tc.format, ""); rec.Code != http.StatusOK {
					t.Errorf("Owner meta %s = %d %s", tc.format, rec.Code, rec.Body.String())
				}
				if got := listed("Cashier", "operation_ref="+opRef(tc.op)); strings.Contains(got, "z_report") {
					t.Errorf("Cashier is offered %s: %s", tc.format, got)
				}
				if rec := serve("Cashier", http.MethodGet, "/print-formats/"+tc.format, ""); rec.Code != http.StatusForbidden {
					t.Errorf("Cashier meta %s = %d %s, want 403", tc.format, rec.Code, rec.Body.String())
				}
			}
			if got := listed("Cashier", ""); !strings.Contains(got, `"sale_receipt"`) || strings.Contains(got, "z_report") {
				t.Errorf("Cashier list = %s", got)
			}

			run("open_session", "k-sess-reopen", `{"register":"REG-2","location":"LOC-1","opening_float":10}`)
			if rows := mySession("Cashier"); len(rows) != 1 || rows[0]["name"] == second {
				t.Fatalf("my_session after close_session + open_session = %v", rows)
			}
			cashierSession := fmt.Sprint(mySession("Cashier")[0]["name"])

			// 10. #407: the Administrator (the shop owner's role) runs the till
			// gate. my_session binds $actor to the Administrator, so it sees only
			// its own open till; the Cashier still sees only theirs.
			admin := doctype.AdminRole
			if rows := mySession(admin); len(rows) != 0 {
				t.Fatalf("Administrator my_session before open = %v", rows)
			}
			seed("Register", "REG-3", map[string]any{"register_name": "Back", "location": "LOC-1", "enabled": true})
			runAs(admin, "open_session", "k-admin-sess", `{"register":"REG-3","location":"LOC-1","opening_float":70}`)
			adminRows := mySession(admin)
			if len(adminRows) != 1 || adminRows[0]["cashier"] != admin || adminRows[0]["register"] != "REG-3" || adminRows[0]["shift"] == nil {
				t.Fatalf("Administrator my_session = %v", adminRows)
			}
			adminSession := fmt.Sprint(adminRows[0]["name"])
			if rows := mySession("Cashier"); len(rows) != 1 || rows[0]["name"] != cashierSession {
				t.Fatalf("Cashier my_session with an Administrator till open = %v", rows)
			}
			runAs(admin, "close_session", "k-admin-close", fmt.Sprintf(`{"shift":%q,"closing_count":70}`, adminRows[0]["shift"]))
			if rows := mySession(admin); len(rows) != 0 {
				t.Fatalf("Administrator my_session after close = %v", rows)
			}
			// Closed tills: if_owner (Cashier) stays own-only; the plain grant
			// (Manager/Owner closed_session) and the Administrator bypass read any
			// till by reference, as the generic Till Session read already allows.
			for _, tc := range []struct {
				role, query, name string
				status, rows      int
			}{
				{admin, "my_closed_session", adminSession, 200, 1},
				{admin, "my_closed_session", session, 200, 1},
				{admin, "closed_session", adminSession, 200, 1},
				{"Cashier", "my_closed_session", adminSession, 200, 0},
				{"Cashier", "my_closed_session", session, 200, 1},
				{"Manager", "closed_session", adminSession, 200, 1},
				{"Owner", "closed_session", adminSession, 200, 1},
				{"Manager", "my_closed_session", adminSession, 403, 0},
				{"Cashier", "closed_session", adminSession, 403, 0},
			} {
				rec := serve(tc.role, http.MethodPost, "/q/pos.core/"+tc.query, fmt.Sprintf(`{"input":{"name":%q}}`, tc.name))
				var out struct {
					Data struct {
						Data []map[string]any `json:"data"`
					} `json:"data"`
				}
				if rec.Code != tc.status || tc.status == 200 && (json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Data.Data) != tc.rows) {
					t.Fatalf("%s %s %s = %d %s, want %d with %d row(s)", tc.role, tc.query, tc.name, rec.Code, rec.Body.String(), tc.status, tc.rows)
				}
				if tc.rows == 1 && out.Data.Data[0]["name"] != tc.name {
					t.Fatalf("%s %s row = %v", tc.role, tc.query, out.Data.Data[0])
				}
			}

			// 11. #91: the Sale detail. Two Cashiers each ring a sale; the Manager
			// rang sale1 earlier. Owner/Manager/Administrator read the items and
			// payments of any sale; a Cashier is refused sale_items/sale_payments
			// (403) and lists only their own sales through my_sales.
			asUser := func(user string, f func()) {
				actingUser = user
				defer func() { actingUser = "" }()
				f()
			}
			sale := func(owner string) string {
				return one("SELECT name FROM "+tbl("Sale")+" WHERE "+dialect.QuoteIdent("owner")+" = ?", owner)
			}
			run("record_sale", "k-detail-a", saleBody(cashierSession, 1, intent("pay-detail-a", 180)))
			saleA := sale("Cashier")
			seed("Register", "REG-4", map[string]any{"register_name": "Annex", "location": "LOC-1", "enabled": true})
			var saleB string
			asUser("Cashier2", func() {
				run("open_session", "k-detail-b-sess", `{"register":"REG-4","location":"LOC-1","opening_float":10}`)
				tillB := fmt.Sprint(mySession("Cashier")[0]["name"])
				run("record_sale", "k-detail-b", saleBody(tillB, 2, intent("pay-detail-b", 360)))
				saleB = sale("Cashier2")
			})
			saleM := sale1
			if saleA == "" || saleB == "" || saleA == saleB {
				t.Fatalf("sales A=%q B=%q", saleA, saleB)
			}
			// queryRows posts a published query and returns its status and rows.
			queryRows := func(role, user, name, input string) (int, []map[string]any, string) {
				t.Helper()
				actingUser = user
				defer func() { actingUser = "" }()
				rec := serve(role, http.MethodPost, "/q/pos.core/"+name, fmt.Sprintf(`{"input":%s}`, input))
				var out struct {
					Data struct {
						Data []map[string]any `json:"data"`
					} `json:"data"`
				}
				if rec.Code == http.StatusOK && json.Unmarshal(rec.Body.Bytes(), &out) != nil {
					t.Fatalf("%s %s body=%s", role, name, rec.Body.String())
				}
				return rec.Code, out.Data.Data, rec.Body.String()
			}
			saleInput := func(sale string) string { return fmt.Sprintf(`{"sale":%q}`, sale) }
			// Detail reads: table-driven over who may see which sale.
			for _, tc := range []struct {
				role, user, query, sale string
				status, rows            int
			}{
				{"Owner", "", "sale_items", saleA, 200, 1},
				{"Owner", "", "sale_items", saleB, 200, 1},
				{"Owner", "", "sale_items", saleM, 200, 1},
				{"Owner", "", "sale_payments", saleA, 200, 1},
				{"Owner", "", "sale_payments", saleB, 200, 1},
				{"Owner", "", "sale_payments", saleM, 200, 1},
				{"Manager", "", "sale_items", saleA, 200, 1},
				{"Manager", "", "sale_items", saleB, 200, 1},
				{"Manager", "", "sale_payments", saleA, 200, 1},
				{"Manager", "", "sale_payments", saleB, 200, 1},
				{"Manager", "", "sale_items", "SALE-404", 200, 0},
				{doctype.AdminRole, "", "sale_items", saleA, 200, 1},
				{doctype.AdminRole, "", "sale_payments", saleB, 200, 1},
				{"Cashier", "", "sale_items", saleA, 403, 0},
				{"Cashier", "", "sale_payments", saleA, 403, 0},
				{"Cashier", "", "sale_items", saleB, 403, 0},
				{"Cashier", "", "sale_payments", saleB, 403, 0},
				{"Cashier", "Cashier2", "sale_items", saleB, 403, 0},
				{"Cashier", "Cashier2", "sale_payments", saleB, 403, 0},
			} {
				status, rows, body := queryRows(tc.role, tc.user, tc.query, saleInput(tc.sale))
				if status != tc.status || status == 200 && len(rows) != tc.rows {
					t.Fatalf("%s %s %s = %d (%d rows) %s, want %d with %d row(s)", tc.role, tc.query, tc.sale, status, len(rows), body, tc.status, tc.rows)
				}
			}
			// The rows carry what the detail page renders.
			_, items, _ := queryRows("Owner", "", "sale_items", saleInput(saleB))
			if len(items) != 1 || items[0]["item"] != "ITM-1" || !decimalIs(items[0]["quantity"], "2") || !decimalIs(items[0]["unit_price"], "180") || !decimalIs(items[0]["amount"], "360") || items[0]["currency"] != "KES" {
				t.Fatalf("sale_items %s = %v", saleB, items)
			}
			_, pays, _ := queryRows("Manager", "", "sale_payments", saleInput(saleB))
			if len(pays) != 1 || pays[0]["method"] != "Cash" || !decimalIs(pays[0]["amount"], "360") || pays[0]["currency"] != "KES" || pays[0]["reference"] == nil || pays[0]["reference"] == "" {
				t.Fatalf("sale_payments %s = %v", saleB, pays)
			}
			// The sale input is required: a missing one is a 400 naming it.
			for _, q := range []string{"sale_items", "sale_payments"} {
				if status, _, body := queryRows("Owner", "", q, `{}`); status != http.StatusBadRequest || !strings.Contains(body, "sale") {
					t.Fatalf("%s without sale = %d %s, want 400", q, status, body)
				}
			}
			// my_sales: each Cashier sees only their own sales (the day's earlier
			// Cashier sales are theirs too), by list and by name.
			for _, tc := range []struct {
				role, user, input string
				status            int
				owner             string   // every returned row is owned by this user
				has, hasNot       []string // sales that must / must not be listed
			}{
				{"Cashier", "", `{}`, 200, "Cashier", []string{saleA}, []string{saleB, saleM}},
				{"Cashier", "Cashier2", `{}`, 200, "Cashier2", []string{saleB}, []string{saleA, saleM}},
				{"Cashier", "", fmt.Sprintf(`{"name":%q}`, saleA), 200, "Cashier", []string{saleA}, []string{saleB, saleM}},
				{"Cashier", "", fmt.Sprintf(`{"name":%q}`, saleB), 200, "", nil, []string{saleA, saleB, saleM}},
				{"Cashier", "", fmt.Sprintf(`{"name":%q}`, saleM), 200, "", nil, []string{saleA, saleB, saleM}},
				{"Cashier", "Cashier2", fmt.Sprintf(`{"name":%q}`, saleA), 200, "", nil, []string{saleA, saleB, saleM}},
				{"Cashier", "Cashier3", `{}`, 200, "", nil, []string{saleA, saleB, saleM}},
				{"Owner", "", `{}`, 403, "", nil, nil},
				{"Manager", "", `{}`, 403, "", nil, nil},
			} {
				status, rows, body := queryRows(tc.role, tc.user, "my_sales", tc.input)
				if status != tc.status {
					t.Fatalf("%s/%s my_sales %s = %d %s, want %d", tc.role, tc.user, tc.input, status, body, tc.status)
				}
				var got []string
				for _, r := range rows {
					got = append(got, fmt.Sprint(r["name"]))
					if r["owner"] != tc.owner || r["register"] == nil || r["total"] == nil || r["status"] == nil {
						t.Fatalf("my_sales %s/%s row = %v, want owner %q", tc.role, tc.user, r, tc.owner)
					}
				}
				for _, n := range tc.has {
					if !slices.Contains(got, n) {
						t.Fatalf("%s/%s my_sales %s = %v, want %s listed", tc.role, tc.user, tc.input, got, n)
					}
				}
				for _, n := range tc.hasNot {
					if slices.Contains(got, n) {
						t.Fatalf("%s/%s my_sales %s = %v leaks %s", tc.role, tc.user, tc.input, got, n)
					}
				}
			}
			// The Administrator bypass reads every sale through my_sales.
			if _, rows, _ := queryRows(doctype.AdminRole, "", "my_sales", `{}`); len(rows) < 3 {
				t.Fatalf("Administrator my_sales rows = %d of %s, want every sale", len(rows), count("Sale"))
			}
			// Neither Cashier gains the unscoped list or direct child reads.
			for _, q := range []string{"recent_sales", "today_totals"} {
				if status, _, body := queryRows("Cashier", "", q, `{}`); status != http.StatusForbidden {
					t.Fatalf("Cashier %s = %d %s, want 403", q, status, body)
				}
			}
		})
	}
}

// decimalIs reports whether a query cell is the exact decimal want.
func decimalIs(v any, want string) bool {
	d, err := decimal.NewFromString(fmt.Sprint(v))
	w, _ := decimal.NewFromString(want)
	return err == nil && d.Equal(w)
}
