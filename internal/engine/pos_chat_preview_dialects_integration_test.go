//go:build integration

package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/api/ai"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/payment"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// TestPOSChatSaleIsPreviewedThenConfirmedAcrossDialects runs chat turns through the real POS
// pack. A till opens and a cash sale records as run_action previews, each confirmed in a later
// turn. A preview leaves no shift, sale, outbox event or number-series count; the second confirm
// replays the first outcome and writes no second sale.
func TestPOSChatSaleIsPreviewedThenConfirmedAcrossDialects(t *testing.T) {
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
			siteName := fmt.Sprintf("pos-chat-preview-%d.example.test", time.Now().UnixNano())
			if err := site.ImportConfig(database, doctype.NewRegistry(), dbName, siteName, filepath.Join("..", "..", "config", "pos"), dialect); err != nil {
				t.Fatalf("import: %v", err)
			}
			info := site.DBSiteInfo{SiteID: siteName, Name: siteName, DBType: tc.driver, DBName: dbName, Status: "active"}
			common := &site.CommonConfig{DBType: tc.driver}
			loaded, err := loadTenantRuntime(context.Background(), tenantRuntimeRequest{info: info, common: common, binaryVersion: "test"}, tenantRuntimeDependencies{
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
			if ierr := install(info, loaded); ierr != nil {
				t.Fatal(ierr)
			}
			services := *loaded.RuntimeServices
			handler := api.NewHandler(loaded.Registry, nil)
			scope := contract.BusinessScope{TenantID: contract.TenantID(siteName), Kind: contract.ScopeKindTenant}
			execution := func(role string) policy.Execution {
				e, eerr := policy.NewVerifiedExecution(contract.TenantID(siteName), contract.ActorContext{
					PrincipalID: role, SubjectUserID: role, PrincipalType: contract.PrincipalHuman, Site: siteName,
					Roles: []string{role}, AuthenticatedAt: time.Now().UTC(),
				})
				if eerr != nil {
					t.Fatal(eerr)
				}
				return e
			}
			// The operations adapter reads the site from a request context, as the chat route does.
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest("POST", "/api/v1/ai/chat", nil)
			c.Set("site_name", siteName)
			c.Set("site_db", database)
			c.Set("site_db_type", tc.driver)
			c.Set("site_registry", loaded.Registry)
			c.Set("site_runtime_services", services)
			c.Set("policy_execution", execution("Manager"))
			ops := api.NewAIModuleOperations(handler, c)

			catalog, err := ops.Catalog(execution("Manager"), scope)
			if err != nil {
				t.Fatalf("catalog: %v", err)
			}
			refFor := func(suffix string) string {
				for _, o := range catalog.PublishedOperations {
					if strings.HasSuffix(o.OperationRef, "~"+suffix) {
						return o.OperationRef
					}
				}
				t.Fatalf("catalog lacks %s", suffix)
				return ""
			}
			openRef, saleRef := refFor("open_session"), refFor("record_sale")

			tbl := func(n string) string { return dialect.QuoteIdent(loaded.Registry.Get(n).RawTableName()) }
			one := func(q string, args ...any) string {
				t.Helper()
				var v string
				if err := database.QueryRow(db.Rebind(dialect, q), args...).Scan(&v); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				return v
			}
			count := func(doc string) string { return one("SELECT COUNT(*) FROM " + tbl(doc)) }
			outboxRows := func() string {
				return one("SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", siteName)
			}
			numbersConsumed := func() string {
				return one("SELECT COALESCE(SUM(current), 0) FROM _kora_naming_series")
			}
			admissions := func() string {
				return one("SELECT COALESCE(SUM(admitted_attempts), 0) FROM _kora_capability_lease WHERE tenant_id = ?", siteName)
			}

			// Administrator-free seeds, as the cashier-day test does.
			seed := func(doc, name string, cols map[string]any) {
				t.Helper()
				seedChatRow(t, database, dialect, loaded.Registry, doc, name, cols)
			}
			seed("Location", "LOC-1", map[string]any{"location_name": "Main"})
			seed("Register", "REG-1", map[string]any{"register_name": "Front", "location": "LOC-1", "enabled": true})
			seed("Item", "ITM-1", map[string]any{"item_name": "Maize", "uom": "Nos", "price": 180, "cost": 120})
			seed("Stock Balance", "SB-1", map[string]any{"item": "ITM-1", "location": "LOC-1", "quantity": 20, "reorder_level": 4})
			payments := &payment.Service{Store: &payment.SQLStore{DB: database, Dialect: dialect}, Providers: payment.NewRegistry()}
			intent := func(key string, amt int64) string {
				in, err := payments.Request(context.Background(), siteName, "Manager", payment.RequestInput{
					Provider: "cash", Amount: decimal.NewFromInt(amt), Currency: "KES", SourceDoctype: "Sale", IdempotencyKey: key,
				})
				if err != nil || in.State != payment.StateConfirmed {
					t.Fatalf("cash intent = %+v err=%v", in, err)
				}
				return in.ID
			}

			tx := &orm.TxManager{
				DB: database, Registry: loaded.Registry, Dialect: dialect, SiteName: siteName,
				Context: ai.WithDialect(context.Background(), dialect), CurrentUser: "Manager", CurrentUserRole: "Manager",
				CurrentUserRoles: []string{"Manager"}, Outbox: outbox.NewSQLWriter(dialect), Execution: execution("Manager"),
				BusinessScope: scope,
			}
			conversation := "conversation-" + fmt.Sprint(time.Now().UnixNano())
			// turn runs one chat turn: one run ID, one conversation.
			turn := func(run string, calls ...map[string]any) string {
				t.Helper()
				anyCalls := make([]any, 0, len(calls))
				for _, call := range calls {
					anyCalls = append(anyCalls, call)
				}
				results := ai.RunToolTurn(tx.Context, tx, loaded.Registry, ai.ToolTurn{
					Calls: anyCalls, Owner: "Manager", Site: siteName, RunID: run, StepID: "step-" + run,
					ConversationID: conversation, Execution: execution("Manager"), Operations: ops,
				})
				content, _ := results[len(results)-1]["content"].(string)
				return content
			}
			runAction := func(id, ref, input string) map[string]any {
				return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "run_action", "arguments": fmt.Sprintf(`{"action":%q,"input":%s}`, ref, input)}}
			}
			confirm := func(id, token string) map[string]any {
				return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "confirm_write", "arguments": fmt.Sprintf(`{"write_id":%q}`, token)}}
			}
			writeIDOf := func(t *testing.T, preview string) string {
				t.Helper()
				var out struct {
					WriteID string `json:"write_id"`
				}
				if err := json.Unmarshal([]byte(preview), &out); err != nil || out.WriteID == "" {
					t.Fatalf("preview %s has no write_id: %v", preview, err)
				}
				return out.WriteID
			}

			// Turn 1 and 2: open the till as a previewed action, then confirm it.
			admissionsBefore := admissions()
			openPreview := turn("run-1", runAction("c1", openRef, `{"register":"REG-1","location":"LOC-1","opening_float":500}`))
			if !strings.Contains(openPreview, `"preview"`) || count("Shift") != "0" || admissions() != admissionsBefore {
				t.Fatalf("open_session preview on %s = %s, shifts=%s, admissions %s->%s; want a preview, no shift and no admission", tc.driver, openPreview, count("Shift"), admissionsBefore, admissions())
			}
			if outcome := turn("run-2", confirm("c2", writeIDOf(t, openPreview))); strings.Contains(outcome, `"error"`) || count("Shift") != "1" {
				t.Fatalf("confirmed open_session on %s = %s, shifts=%s; want one shift", tc.driver, outcome, count("Shift"))
			}
			till := one("SELECT name FROM " + tbl("Till Session"))

			// Turn 3: the sale previews. Nothing is kept: no sale, no outbox event, no number consumed.
			saleInput := fmt.Sprintf(`{"register":"REG-1","location":"LOC-1","till_session":%q,"currency":"KES","items":[{"item":"ITM-1","quantity":2,"unit_price":180,"amount":360}],"payments":[{"method":"Cash","amount":360,"currency":"KES","reference":%q}]}`, till, intent("pay-1", 360))
			outboxBefore, numbersBefore := outboxRows(), numbersConsumed()
			salePreview := turn("run-3", runAction("c3", saleRef, saleInput))
			if !strings.Contains(salePreview, `"preview"`) {
				t.Fatalf("record_sale preview on %s = %s, want a preview", tc.driver, salePreview)
			}
			admissionsBeforeSale := admissions()
			if count("Sale") != "0" || outboxRows() != outboxBefore || numbersConsumed() != numbersBefore || admissions() != admissionsBeforeSale {
				t.Fatalf("record_sale preview on %s left sales=%s outbox %s->%s numbers %s->%s admissions %s->%s; want none", tc.driver, count("Sale"), outboxBefore, outboxRows(), numbersBefore, numbersConsumed(), admissionsBeforeSale, admissions())
			}

			// Turn 4: the person says yes. One sale is written, and a repeated confirm replays it.
			saleToken := writeIDOf(t, salePreview)
			first := turn("run-4", confirm("c4", saleToken))
			if strings.Contains(first, `"error"`) || count("Sale") != "1" || admissions() == admissionsBeforeSale {
				t.Fatalf("confirmed record_sale on %s = %s, sales=%s; want one sale through full admission", tc.driver, first, count("Sale"))
			}
			if strings.Contains(first, `"error"`) || count("Sale") != "1" {
				t.Fatalf("confirmed record_sale on %s = %s, sales=%s; want one sale", tc.driver, first, count("Sale"))
			}
			if replay := turn("run-5", confirm("c5", saleToken)); replay != first || count("Sale") != "1" {
				t.Fatalf("replayed confirm on %s = %s, sales=%s; want the first outcome and one sale", tc.driver, replay, count("Sale"))
			}
		})
	}
}
