//go:build integration

package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/envcompat"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/payment"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
)

// Pack actions through the chat, on the real POS pack. A scripted model reads the till actions with list_actions,
// asks the person to confirm, and runs them with run_action after the person's yes. The chat opens a till and
// records a cash sale; the POS writes one till session and one sale. Then the same operation is sent again with the
// same idempotency key, and the POS writes nothing new.
const (
	packChatOpenTill   = "pos.core~pos.drawer~open_session"
	packChatRecordSale = "pos.core~pos.checkout~record_sale"
	packChatUser       = "shift-manager"
)

// packChatStep is one scripted provider answer. args, when set, is computed as the answer is served, so a step can
// name a row the earlier steps wrote.
type packChatStep struct {
	tool, content string
	args          func() string
}

// packChatModel is the scripted provider. It answers each request with the next step and keeps every request body.
type packChatModel struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	steps  []packChatStep
	bodies []string
}

func startPackChatModel(t *testing.T) *packChatModel {
	t.Helper()
	m := &packChatModel{t: t}
	m.server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.server.Close)
	return m
}

func (m *packChatModel) script(steps []packChatStep) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.steps) > 0 {
		m.t.Errorf("%d scripted steps were not used", len(m.steps))
	}
	m.steps = steps
}

func (m *packChatModel) served() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.bodies)
}

func (m *packChatModel) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bodies = append(m.bodies, string(raw))
	if len(m.steps) == 0 {
		m.t.Errorf("the model was asked again with nothing scripted")
		http.Error(w, "nothing scripted", http.StatusInternalServerError)
		return
	}
	step := m.steps[0]
	m.steps = m.steps[1:]
	message := map[string]any{"role": "assistant", "content": step.content}
	finish := "stop"
	if step.tool != "" {
		finish = "tool_calls"
		args := "{}"
		if step.args != nil {
			args = step.args()
		}
		message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
			"id": fmt.Sprintf("call_%d", len(m.bodies)), "type": "function",
			"function": map[string]any{"name": step.tool, "arguments": args},
		}}}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20},
	})
}

func TestPackActionsChatAcrossDialects(t *testing.T) {
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
			siteName := fmt.Sprintf("pack-chat-%d.example.test", time.Now().UnixNano())
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
			if err := install(info, loaded); err != nil {
				t.Fatal(err)
			}
			services := *loaded.RuntimeServices
			execution, err := policy.NewVerifiedExecution(contract.TenantID(siteName), contract.ActorContext{
				PrincipalID: packChatUser, SubjectUserID: packChatUser, PrincipalType: contract.PrincipalHuman, Site: siteName,
				Roles: []string{"Manager"}, AuthenticatedAt: time.Now().UTC(),
			})
			if err != nil {
				t.Fatal(err)
			}

			tbl := func(name string) string { return dialect.QuoteIdent(loaded.Registry.Get(name).RawTableName()) }
			one := func(q string, args ...any) string {
				t.Helper()
				var v string
				if err := database.QueryRow(db.Rebind(dialect, q), args...).Scan(&v); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				return v
			}
			count := func(doc string) string { return one("SELECT COUNT(*) FROM " + tbl(doc)) }
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
			cashIntent := func(key string) string {
				t.Helper()
				in, err := payments.Request(context.Background(), siteName, "Manager", payment.RequestInput{
					Provider: "cash", Amount: decimal.NewFromInt(180), Currency: "KES", SourceDoctype: "Sale", IdempotencyKey: key,
				})
				if err != nil || in.State != payment.StateConfirmed {
					t.Fatalf("cash intent = %+v err=%v", in, err)
				}
				return in.ID
			}
			saleIntent := cashIntent("pack-chat-intent-1")

			model := startPackChatModel(t)
			t.Setenv("KORA_CLOUD_AI_GATEWAY_URL", model.server.URL)
			t.Setenv("KORA_CLOUD_AI_GATEWAY_MODEL", "")
			t.Setenv(envcompat.IntegrationTokenEnv, "pack-chat-token")

			handler := api.NewHandler(loaded.Registry, nil)
			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Set("site_name", siteName)
				c.Set("site_id", siteName)
				c.Set("site_db", database)
				c.Set("site_db_type", tc.driver)
				c.Set("site_registry", loaded.Registry)
				c.Set("site_runtime_services", services)
				c.Set("policy_execution", execution)
			})
			router.POST("/api/v1/ai/chat", handler.HandleChat)
			router.POST("/op/:operation", handler.HandleExecutePublishedOperation)

			thread := "pack-chat-" + tc.driver
			chat := func(message string, steps []packChatStep) []byte {
				t.Helper()
				model.script(steps)
				before := model.served()
				body, _ := json.Marshal(map[string]any{"message": message, "context": map[string]any{"threadId": thread}})
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/ai/chat", bytes.NewReader(body)))
				if rec.Code != http.StatusOK {
					t.Fatalf("chat %q = %d: %s", message, rec.Code, rec.Body)
				}
				if got := model.served() - before; got != len(steps) {
					t.Fatalf("chat %q: model answered %d times, want %d", message, got, len(steps))
				}
				return rec.Body.Bytes()
			}

			// Turn 1: the model lists the till actions and asks for confirmation.
			chat("Open my till with 500 float at Front.", []packChatStep{
				{tool: "list_actions", args: func() string { return `{"record_type":"Till Session"}` }},
				{content: "Open a till on REG-1 with a 500 float? Reply yes to confirm."},
			})
			if n := count("Till Session"); n != "0" {
				t.Fatalf("till sessions before confirmation = %s, want 0", n)
			}
			// Turn 2: after yes, the model runs the action, and the till opens.
			chat("yes", []packChatStep{
				{tool: "run_action", args: func() string {
					return fmt.Sprintf(`{"action":%q,"input":{"register":"REG-1","location":"LOC-1","opening_float":500}}`, packChatOpenTill)
				}},
				{content: "Your till is open."},
			})
			if n := count("Till Session"); n != "1" {
				t.Fatalf("till sessions after run_action = %s, want 1", n)
			}

			// Turn 3: the model describes the sale action before it asks for confirmation.
			chat("Sell one Maize, paid in cash.", []packChatStep{
				{tool: "list_actions", args: func() string { return fmt.Sprintf(`{"action":%q}`, packChatRecordSale) }},
				{content: "Sell 1 Maize for 180 KES in cash? Reply yes to confirm."},
			})
			// Turn 4: after yes, the model records the sale through run_action.
			saleInput := func() string {
				till := one("SELECT name FROM " + tbl("Till Session"))
				return fmt.Sprintf(`{"action":%q,"input":{"register":"REG-1","location":"LOC-1","till_session":%q,"currency":"KES","items":[{"item":"ITM-1","quantity":1,"unit_price":180,"amount":180}],"payments":[{"method":"Cash","amount":180,"currency":"KES","reference":%q}]}}`, packChatRecordSale, till, saleIntent)
			}
			chat("yes", []packChatStep{
				{tool: "run_action", args: saleInput},
				{content: "Sale recorded."},
			})
			if n := count("Sale"); n != "1" {
				t.Fatalf("sales after run_action = %s, want 1", n)
			}
			// The provider was offered the two pack-action tools and the run answered "completed".
			if first := model.bodies[0]; !strings.Contains(first, `"name":"run_action"`) || !strings.Contains(first, `"name":"list_actions"`) {
				t.Fatalf("pack-action tools were not offered: %.300s", first)
			}
			if last := model.bodies[len(model.bodies)-1]; !strings.Contains(last, `\"status\":\"completed\"`) {
				t.Fatalf("the sale result did not reach the model as completed: %.600s", last)
			}

			// Replay: the same operation with the same key, sent again, writes nothing and returns the first receipt.
			replayBody := func(intent string) string {
				till := one("SELECT name FROM " + tbl("Till Session"))
				return fmt.Sprintf(`{"meta":{"idempotency_key":"pack-chat-replay"},"input_schema":{"id":"pos.core.record_sale.input","version":1},"input":{"register":"REG-1","location":"LOC-1","till_session":%q,"currency":"KES","items":[{"item":"ITM-1","quantity":1,"unit_price":180,"amount":180}],"payments":[{"method":"Cash","amount":180,"currency":"KES","reference":%q}]}}`, till, intent)
			}
			replayIntent := cashIntent("pack-chat-intent-2")
			post := func() (int, string) {
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/op/"+packChatRecordSale, strings.NewReader(replayBody(replayIntent))))
				var out struct {
					Data contract.OperationReceipt `json:"data"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &out)
				return rec.Code, string(out.Data.ExecutionID)
			}
			code, first := post()
			if code != http.StatusOK || first == "" {
				t.Fatalf("first replay send = %d", code)
			}
			code, second := post()
			if code != http.StatusOK || second != first {
				t.Fatalf("replay = %d execution %q, want %q", code, second, first)
			}
			if n := count("Sale"); n != "2" {
				t.Fatalf("sales after replay = %s, want 2 (one from the chat, one from the replayed key)", n)
			}
		})
	}
}
