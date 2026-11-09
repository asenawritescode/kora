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

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
)

// Acceptance test (#355): a shop records a payment received on a configured
// till/paybill with the confirmation code, through published operations and
// the payment-intent flow only. Reports keep it apart from cash.
func TestPOSManualPaymentsAcrossDialects(t *testing.T) {
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
			if err := site.BootstrapSystemTables(context.Background(), database, dialect); err != nil {
				t.Fatal(err)
			}
			siteName := fmt.Sprintf("pos-manual-%d.example.test", time.Now().UnixNano())
			if err := site.ImportConfig(database, doctype.NewRegistry(), dbName, siteName, filepath.Join("..", "..", "config", "pos"), dialect); err != nil {
				t.Fatalf("import: %v", err)
			}
			info := site.DBSiteInfo{SiteID: siteName, Name: siteName, DBType: tc.driver, DBName: dbName, Status: "active"}
			common := &site.CommonConfig{DBType: tc.driver}
			loaded, err := loadTenantRuntime(context.Background(), info, common, nil, "test", tenantRuntimeDependencies{
				connect:   func(context.Context, *site.Config) (*sql.DB, error) { return database, nil },
				bootstrap: func(context.Context, *sql.DB, db.Dialect) error { return nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			install, err := NewTenantServiceInstaller(TenantServiceConfig{
				Common: common, KernelCommands: kernel.NewCommandRegistry(),
				ResolveStorage: func(*site.Config) (storage.Backend, error) {
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

			acting := "Cashier"
			router := gin.New()
			router.Use(func(c *gin.Context) {
				e, err := policy.NewVerifiedExecution(contract.TenantID(siteName), contract.ActorContext{
					PrincipalID: acting, SubjectUserID: acting, PrincipalType: contract.PrincipalHuman, Site: siteName,
					Roles: []string{acting}, AuthenticatedAt: time.Now().UTC(),
				})
				if err != nil {
					t.Fatal(err)
				}
				c.Set("site_name", siteName)
				c.Set("site_db", database)
				c.Set("site_db_type", tc.driver)
				c.Set("site_registry", loaded.Registry)
				c.Set("site_runtime_services", services)
				c.Set("policy_execution", e)
			})
			router.GET("/catalog", handler.HandleApplicationCatalog)
			router.GET("/print/:format_name", api.NewSitePrintHandler(handler))
			router.POST("/op/:operation", handler.HandleExecutePublishedOperation)
			router.POST("/q/:module/:query", handler.HandlePublishedModuleQuery)
			router.GET("/r/:doctype", handler.HandleList)
			router.POST("/r/:doctype", handler.HandleCreate)
			router.POST("/payments", handler.HandlePaymentRequest)
			router.GET("/payments/:id", handler.HandlePaymentGet)
			serve := func(role, method, path, body string, headers ...string) *httptest.ResponseRecorder {
				acting = role
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				for i := 0; i+1 < len(headers); i += 2 {
					req.Header.Set(headers[i], headers[i+1])
				}
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
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
			payTbl := dialect.QuoteIdent(loaded.Registry.Get("Sale").RawChildTableName("payments"))
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
			seed("Stock Balance", "SB-1", map[string]any{"item": "ITM-1", "location": "LOC-1", "quantity": 50, "reorder_level": 4})

			rec := serve("Cashier", http.MethodGet, "/catalog", "")
			var cat struct {
				Data contract.ApplicationCatalog `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &cat); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("catalog %d %s", rec.Code, rec.Body.String())
			}
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
			tryOp := func(role, name, key, input string) *httptest.ResponseRecorder {
				t.Helper()
				for _, o := range cat.Data.PublishedOperations {
					if strings.HasSuffix(o.OperationRef, "~"+name) {
						return serve(role, http.MethodPost, "/op/"+o.OperationRef, fmt.Sprintf(`{"meta":{"idempotency_key":%q},"input_schema":{"id":%q,"version":%d},"input":%s}`, key, o.InputSchema.ID, o.InputSchema.Version, input))
					}
				}
				t.Fatalf("catalog lacks %s", name)
				return nil
			}
			completed := func(rec *httptest.ResponseRecorder) bool {
				return rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), `"completed"`)
			}
			runOp := func(role, name, key, input string) {
				t.Helper()
				if rec := tryOp(role, name, key, input); !completed(rec) {
					t.Fatalf("%s as %s = %d %s", name, role, rec.Code, rec.Body.String())
				}
			}

			// 1. Accounts are Owner/Manager data; the Cashier only reads them.
			account := func(role, name, typ, number, currency string, enabled int) *httptest.ResponseRecorder {
				return serve(role, http.MethodPost, "/r/Payment%20Account",
					fmt.Sprintf(`{"account_name":%q,"account_type":%q,"number":%q,"currency":%q,"enabled":%d}`, name, typ, number, currency, enabled))
			}
			if rec := account("Cashier", "Cashier till", "Till", "999", "KES", 1); rec.Code != http.StatusForbidden {
				t.Fatalf("Cashier created an account = %d %s", rec.Code, rec.Body.String())
			}
			// Cash-only tenant: with no account configured the manual provider has nothing to record against.
			intent := func(role, key, body string) (int, string, map[string]any) {
				t.Helper()
				rec := serve(role, http.MethodPost, "/payments", body, "Idempotency-Key", key)
				var out struct {
					Data map[string]any `json:"data"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &out)
				return rec.Code, rec.Body.String(), out.Data
			}
			manualBody := func(acct, code, currency string) string {
				return fmt.Sprintf(`{"provider":"manual","amount":"180","currency":%q,"source_doctype":"Sale","operation_ref":%q,"payment_account":%q,"reference":%q}`, currency, opRef("record_sale"), acct, code)
			}
			if code, body, _ := intent("Cashier", "m-none", manualBody("ACC-NONE", "QK1", "KES")); code != http.StatusBadRequest || !strings.Contains(body, "payment account not found") {
				t.Fatalf("manual intent with no account = %d %s", code, body)
			}
			for _, a := range []struct{ name, typ, number, currency string }{{"Shop till", "Till", "123456", "KES"}, {"Shop paybill", "Paybill", "400200", "KES"}} {
				if rec := account("Owner", a.name, a.typ, a.number, a.currency, 1); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
					t.Fatalf("Owner create account %s = %d %s", a.name, rec.Code, rec.Body.String())
				}
			}
			if rec := account("Manager", "Bank KES", "Bank", "0011", "KES", 0); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
				t.Fatalf("Manager create account = %d %s", rec.Code, rec.Body.String())
			}
			if rec := account("Owner", "Dollar till", "Till", "777", "USD", 1); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
				t.Fatalf("Owner create account = %d %s", rec.Code, rec.Body.String())
			}
			if rec := serve("Cashier", http.MethodGet, "/r/Payment%20Account", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "123456") {
				t.Fatalf("Cashier reads accounts = %d %s", rec.Code, rec.Body.String())
			}
			acct := func(label string) string {
				return one("SELECT name FROM "+tbl("Payment Account")+" WHERE account_name = ?", label)
			}
			till, paybill, bank, usd := acct("Shop till"), acct("Shop paybill"), acct("Bank KES"), acct("Dollar till")

			// 2. Intent creation: every refusal is a clear 400 and stores nothing.
			before := one("SELECT COUNT(*) FROM _kora_payment_intent")
			refusals := []struct{ name, body, want string }{
				{"missing code", manualBody(till, "", "KES"), "confirmation code is required"},
				{"missing account", manualBody("", "QK1", "KES"), "payment account is required"},
				{"unknown account", manualBody("ACC-NOPE", "QK1", "KES"), "payment account not found"},
				{"disabled account", manualBody(bank, "QK1", "KES"), "payment account is disabled"},
				{"wrong currency", manualBody(usd, "QK1", "KES"), "currency does not match"},
			}
			for i, r := range refusals {
				if code, body, _ := intent("Cashier", fmt.Sprintf("m-ref-%d", i), r.body); code != http.StatusBadRequest || !strings.Contains(body, r.want) {
					t.Errorf("%s = %d %s, want 400 %q", r.name, code, body, r.want)
				}
			}
			if after := one("SELECT COUNT(*) FROM _kora_payment_intent"); after != before {
				t.Fatalf("refused manual requests stored %s intents", after)
			}

			// 3. A valid code is recorded, unverified, with its account; the same
			// code is refused for the same account (case-insensitively) but is a
			// different payment on another account.
			code, body, tillIntent := intent("Cashier", "m-ok-1", manualBody(till, "qk1a2b3c4d", "KES"))
			if code != http.StatusOK || tillIntent["state"] != "confirmed" || tillIntent["verification"] != "manually_recorded" ||
				tillIntent["payment_account"] != till || tillIntent["provider_reference"] != "QK1A2B3C4D" {
				t.Fatalf("manual intent = %d %s", code, body)
			}
			if code, body, _ := intent("Cashier", "m-dup", manualBody(till, "QK1A2B3C4D", "KES")); code != http.StatusBadRequest || !strings.Contains(body, "already recorded") {
				t.Fatalf("duplicate code = %d %s", code, body)
			}
			if code, body, _ := intent("Cashier", "m-ok-1", manualBody(till, "qk1a2b3c4d", "KES")); code != http.StatusOK {
				t.Fatalf("idempotent replay = %d %s", code, body)
			}
			code, body, paybillIntent := intent("Cashier", "m-ok-2", manualBody(paybill, "QK1A2B3C4D", "KES"))
			if code != http.StatusOK || paybillIntent["payment_account"] != paybill {
				t.Fatalf("same code on another account = %d %s", code, body)
			}
			// A cash intent is unchanged: no account, no verification label.
			code, body, cashIntent := intent("Cashier", "c-1", fmt.Sprintf(`{"provider":"cash","amount":"360","currency":"KES","source_doctype":"Sale","operation_ref":%q}`, opRef("record_sale")))
			if code != http.StatusOK || cashIntent["verification"] != "" || cashIntent["payment_account"] != "" {
				t.Fatalf("cash intent = %d %s", code, body)
			}

			// 4. Sales: the Cashier records a mobile-money sale and a cash sale.
			runOp("Cashier", "open_session", "k-session", `{"register":"REG-1","location":"LOC-1","opening_float":500}`)
			session := one("SELECT name FROM " + tbl("Till Session"))
			sale := func(payments string, qty int) string {
				return fmt.Sprintf(`{"register":"REG-1","location":"LOC-1","till_session":%q,"currency":"KES","items":[{"item":"ITM-1","quantity":%d,"unit_price":180,"amount":%d}],"payments":[%s]}`, session, qty, qty*180, payments)
			}
			mobileLine := func(account, ref string) string {
				return fmt.Sprintf(`{"method":"Mobile Money","amount":180,"currency":"KES","reference":%q,"payment_account":%q,"verification":"manually_recorded"}`, ref, account)
			}
			runOp("Cashier", "record_sale", "k-sale-mm", sale(mobileLine(till, fmt.Sprint(tillIntent["id"])), 1))
			if got := one("SELECT expected_cash FROM " + tbl("Till Session")); !strings.HasPrefix(got, "500") {
				t.Fatalf("expected cash after a mobile-money sale = %s, want 500 (cash only counts)", got)
			}
			if got := one("SELECT payment_account FROM "+payTbl+" WHERE method = ?", "Mobile Money"); got != till {
				t.Fatalf("stored payment_account = %q, want %q", got, till)
			}
			if got := one("SELECT verification FROM "+payTbl+" WHERE method = ?", "Mobile Money"); got != "manually_recorded" {
				t.Fatalf("stored verification = %q", got)
			}
			// An intent is single-use even for manual payments.
			if rec := tryOp("Cashier", "record_sale", "k-sale-reuse", sale(mobileLine(till, fmt.Sprint(tillIntent["id"])), 1)); completed(rec) {
				t.Fatalf("a manual intent paid two sales: %s", rec.Body.String())
			}
			// A mixed sale: cash and paybill.
			mixed := fmt.Sprintf(`{"method":"Cash","amount":360,"currency":"KES","reference":%q},`, fmt.Sprint(cashIntent["id"]))
			code, body, paybill2 := intent("Cashier", "m-ok-3", manualBody(paybill, "QK9Z8Y7X6W", "KES"))
			if code != http.StatusOK {
				t.Fatalf("paybill intent = %d %s", code, body)
			}
			runOp("Cashier", "record_sale", "k-sale-mixed", sale(mixed+mobileLine(paybill, fmt.Sprint(paybill2["id"])), 3))
			if got := one("SELECT expected_cash FROM " + tbl("Till Session")); !strings.HasPrefix(got, "860") {
				t.Fatalf("expected cash = %s, want 860 (500 + 360 cash)", got)
			}

			// 5. Reports: by method as before; by account separates manual payments.
			type row = map[string]any
			query := func(role, name string) (int, []row) {
				t.Helper()
				rec := serve(role, http.MethodPost, "/q/pos.core/"+name, fmt.Sprintf(`{"input":{"till_session":%q}}`, session))
				var out struct {
					Data struct {
						Data []row `json:"data"`
					} `json:"data"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &out)
				return rec.Code, out.Data.Data
			}
			if status, _ := query("Cashier", "payments_by_account"); status != http.StatusForbidden {
				t.Fatalf("Cashier payments_by_account = %d, want 403", status)
			}
			for _, role := range []string{"Owner", "Manager"} {
				status, rows := query(role, "payments_by_account")
				if status != http.StatusOK || len(rows) != 2 {
					t.Fatalf("%s payments_by_account = %d %v", role, status, rows)
				}
				totals := map[string]string{}
				for _, r := range rows {
					if r["method"] != "Mobile Money" || r["verification"] != "manually_recorded" {
						t.Fatalf("by-account row = %v", r)
					}
					totals[fmt.Sprint(r["payment_account"])] = fmt.Sprint(r["total"])
				}
				if !strings.HasPrefix(totals[till], "180") || !strings.HasPrefix(totals[paybill], "180") {
					t.Fatalf("by-account totals = %v", totals)
				}
				_, byMethod := query(role, "payments_by_method")
				methods := map[string]string{}
				for _, r := range byMethod {
					methods[fmt.Sprint(r["method"])] = fmt.Sprint(r["total"])
				}
				if !strings.HasPrefix(methods["Cash"], "360") || !strings.HasPrefix(methods["Mobile Money"], "360") {
					t.Fatalf("by-method totals = %v", methods)
				}
			}
			// The Z report reprint lists the by-account section and labels the manual amounts.
			for _, as := range []string{"text", "html"} {
				rec := serve("Owner", http.MethodGet, "/print/z_report_reprint?as="+as+"&record=Till%20Session&name="+session, "")
				got := rec.Body.String()
				if rec.Code != http.StatusOK || !strings.Contains(got, till) || !strings.Contains(got, paybill) || !strings.Contains(got, "manually recorded") {
					t.Errorf("Z report reprint as %s = %d\n%s", as, rec.Code, got)
				}
			}
		})
	}
}
