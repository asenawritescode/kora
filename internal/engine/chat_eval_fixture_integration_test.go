//go:build integration || liveeval

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
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/api/ai"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/envcompat"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/payment"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
)

// This file is the chat harness shared by the scripted record-task evals (CI) and the live evals (planner). A
// tenant is loaded from the POS pack on its own database, a gin router serves /api/v1/ai/chat through HandleChat,
// and a scripted OpenAI-format server answers each model call with the next scripted step.

// packChatUser is the signed-in person of the chat fixtures: a manager, and the cashier of the seeded till.
const packChatUser = "shift-manager"

// packChatStep is one scripted provider answer. args, when set, is computed as the answer is served, so a step can
// name a row the earlier steps wrote or a write_id listed by the request.
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

// packWriteIDPattern matches one open-preview line of a request, "write_id pw_...: summary", up to the JSON line break
// that ends it. The summary can hold escaped quotes, so the match stops at the line break, not at a backslash.
var packWriteIDPattern = regexp.MustCompile(`write_id (pw_[0-9A-Za-z]+): (.*?)\\n`)

// writeIDIn returns the first write_id listed in a request body, or "" when none is listed.
func writeIDIn(body string) string {
	if m := packWriteIDPattern.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

// writeIDFor returns the write_id of the open preview whose summary contains needle, or "" when none does.
func writeIDFor(body, needle string) string {
	for _, m := range packWriteIDPattern.FindAllStringSubmatch(body, -1) {
		if strings.Contains(m[2], needle) {
			return m[1]
		}
	}
	return ""
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

// lastBody returns the request the model is answering. Only a step's args function may call it: serve holds the
// lock while it runs args.
func (m *packChatModel) lastBody() string {
	return m.bodies[len(m.bodies)-1]
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

// isolatedDB gives the run its own MySQL database or Postgres database because
// pack imports write global per-database DocType rows.
func isolatedDB(t *testing.T, driver, dsn string) (*sql.DB, string) {
	t.Helper()
	admin, err := sql.Open(driver, dsn)
	if err != nil || admin.Ping() != nil {
		t.Fatalf("connect %s: %v", driver, err)
	}
	name := fmt.Sprintf("kora_packs_%x", time.Now().UnixNano())
	var target string
	if driver == "postgres" {
		if _, err = admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// Best effort: the database is disposable and the test has finished with it.
			_, _ = admin.Exec(`DROP DATABASE "` + name + `" WITH (FORCE)`)
			_ = admin.Close()
		})
		u, _ := url.Parse(dsn)
		u.Path = "/" + name
		target = u.String()
	} else {
		if _, err = admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// Best effort: the database is disposable and the test has finished with it.
			_, _ = admin.Exec("DROP DATABASE `" + name + "`")
			_ = admin.Close()
		})
		var cfg *mysql.Config
		cfg, err = mysql.ParseDSN(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.DBName = name // DSNs naming a database must not be shared across tenants
		target = cfg.FormatDSN()
	}
	conn, err := sql.Open(driver, target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, name
}

// seedChatRow inserts one record of a doctype with the given field values.
func seedChatRow(t *testing.T, database *sql.DB, dialect db.Dialect, registry *doctype.Registry, doc, name string, cols map[string]any) {
	t.Helper()
	keys, marks, args := []string{dialect.QuoteIdent("name")}, []string{"?"}, []any{name}
	for k, v := range cols {
		keys, marks, args = append(keys, dialect.QuoteIdent(k)), append(marks, "?"), append(args, v)
	}
	table := dialect.QuoteIdent(registry.Get(doc).RawTableName())
	q := "INSERT INTO " + table + " (" + strings.Join(keys, ",") + ") VALUES (" + strings.Join(marks, ",") + ")"
	if _, err := database.Exec(db.Rebind(dialect, q), args...); err != nil {
		t.Fatalf("seed %s: %v", doc, err)
	}
}

// chatFixture is one tenant wired for the chat: its database and registry, a router that serves /api/v1/ai/chat
// through HandleChat as packChatUser, and the scripted model that stands in for the provider.
type chatFixture struct {
	t        *testing.T
	driver   string
	dialect  db.Dialect
	database *sql.DB
	registry *doctype.Registry
	siteName string
	router   *gin.Engine
	model    *packChatModel
	payments *payment.Service
	thread   string
	// cases counts the resets, so each case has its own conversation and keys.
	cases int
}

// newChatFixture loads the POS pack on a fresh database and wires the chat to the scripted model. Build one fixture
// per dialect and call reset before each case: loading the pack is the slow part.
func newChatFixture(t *testing.T, driver, dsn string) *chatFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	database, dbName := isolatedDB(t, driver, dsn)
	dialect := db.Resolve(driver)
	if err := site.BootstrapSystemTables(context.Background(), database, dialect); err != nil {
		t.Fatal(err)
	}
	siteName := fmt.Sprintf("chat-fixture-%d.example.test", time.Now().UnixNano())
	if err := site.ImportConfig(database, doctype.NewRegistry(), dbName, siteName, "../../config/pos", dialect); err != nil {
		t.Fatalf("import: %v", err)
	}
	info := site.DBSiteInfo{SiteID: siteName, Name: siteName, DBType: driver, DBName: dbName, Status: "active"}
	common := &site.CommonConfig{DBType: driver}
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
	if err = install(info, loaded); err != nil {
		t.Fatal(err)
	}
	services := *loaded.RuntimeServices
	execution, err := policy.NewVerifiedExecution(contract.TenantID(siteName), contract.ActorContext{
		PrincipalID: packChatUser, SubjectUserID: packChatUser, PrincipalType: contract.PrincipalHuman, Site: siteName,
		// Manager runs tills and sales; Inventory Manager writes Items (config/inventory/permissions.yaml).
		Roles: []string{"Manager", "Inventory Manager"}, AuthenticatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &chatFixture{
		t: t, driver: driver, dialect: dialect, database: database, registry: loaded.Registry, siteName: siteName,
		model:    startPackChatModel(t),
		payments: &payment.Service{Store: &payment.SQLStore{DB: database, Dialect: dialect}, Providers: payment.NewRegistry()},
		thread:   "chat-fixture-" + driver,
	}
	handler := api.NewHandler(loaded.Registry, nil)
	f.router = gin.New()
	f.router.Use(func(c *gin.Context) {
		c.Set("site_name", siteName)
		c.Set("site_id", siteName)
		c.Set("site_db", database)
		c.Set("site_db_type", driver)
		c.Set("site_registry", loaded.Registry)
		c.Set("site_runtime_services", services)
		c.Set("policy_execution", execution)
	})
	f.router.POST("/api/v1/ai/chat", handler.HandleChat)
	f.router.POST("/op/:operation", handler.HandleExecutePublishedOperation)
	return f
}

// reset makes the tenant ready for one case: every doctype table is emptied, the site's open previews and usage
// rows are dropped, the case's seed is inserted, and the next chat turn starts a new conversation. The pack and the
// provider wiring stay, so a case costs the seed and its own turns, not a new tenant.
func (f *chatFixture) reset(seed func(*chatFixture)) {
	f.t.Helper()
	ctx := ai.WithDialect(context.Background(), f.dialect)
	if err := ai.EnsureAIRunTables(ctx, f.database, f.dialect); err != nil {
		f.t.Fatalf("ensure AI tables: %v", err)
	}
	for _, dt := range f.registry.All() {
		if _, err := f.database.Exec("DELETE FROM " + f.dialect.QuoteIdent(dt.RawTableName())); err != nil {
			f.t.Fatalf("clear %s: %v", dt.Name, err)
		}
	}
	for _, q := range []string{"DELETE FROM _kora_ai_pending_write WHERE site = ?", "DELETE FROM _kora_ai_usage WHERE site = ?"} {
		if _, err := f.database.Exec(db.Rebind(f.dialect, q), f.siteName); err != nil {
			f.t.Fatalf("clear %s: %v", q, err)
		}
	}
	f.cases++
	f.thread = fmt.Sprintf("chat-fixture-%s-%d", f.driver, f.cases)
	seed(f)
}

// useScriptedGateway points the chat at the scripted model through the Cloud gateway route.
func (f *chatFixture) useScriptedGateway() {
	f.t.Setenv("KORA_CLOUD_AI_GATEWAY_URL", f.model.server.URL)
	f.t.Setenv("KORA_CLOUD_AI_GATEWAY_MODEL", "")
	f.t.Setenv(envcompat.IntegrationTokenEnv, "chat-fixture-token")
}

// tbl returns the quoted table of a doctype.
func (f *chatFixture) tbl(doc string) string {
	return f.dialect.QuoteIdent(f.registry.Get(doc).RawTableName())
}

// one runs a query that returns one text value.
func (f *chatFixture) one(q string, args ...any) string {
	f.t.Helper()
	var v string
	if err := f.database.QueryRow(db.Rebind(f.dialect, q), args...).Scan(&v); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
	return v
}

// count returns the number of rows of a doctype.
func (f *chatFixture) count(doc string) string {
	return f.one("SELECT COUNT(*) FROM " + f.tbl(doc))
}

// seed inserts one record of a doctype.
func (f *chatFixture) seed(doc, name string, cols map[string]any) {
	f.t.Helper()
	seedChatRow(f.t, f.database, f.dialect, f.registry, doc, name, cols)
}

// cashIntent records a confirmed cash payment intent and returns its ID.
func (f *chatFixture) cashIntent(key string, amount int64) string {
	f.t.Helper()
	in, err := f.payments.Request(context.Background(), f.siteName, "Manager", payment.RequestInput{
		Provider: "cash", Amount: decimal.NewFromInt(amount), Currency: "KES", SourceDoctype: "Sale", IdempotencyKey: key,
	})
	if err != nil || in.State != payment.StateConfirmed {
		f.t.Fatalf("cash intent = %+v err=%v", in, err)
	}
	return in.ID
}

// openPreviews counts the previews that are still open for the site.
func (f *chatFixture) openPreviews() int {
	var n int
	if err := f.database.QueryRow(db.Rebind(f.dialect, "SELECT COUNT(*) FROM _kora_ai_pending_write WHERE site = ? AND state = ?"), f.siteName, "previewed").Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// post sends one chat turn and returns the HTTP status and body.
func (f *chatFixture) post(message string) (int, []byte) {
	body, _ := json.Marshal(map[string]any{"message": message, "context": map[string]any{"threadId": f.thread}})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/ai/chat", bytes.NewReader(body)))
	return rec.Code, rec.Body.Bytes()
}

// postOperation sends one published operation with its input and returns the HTTP status and the receipt's
// execution ID, which a replay repeats.
func (f *chatFixture) postOperation(operation, input string) (int, string) {
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/op/"+operation, strings.NewReader(input)))
	var out struct {
		Data contract.OperationReceipt `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, string(out.Data.ExecutionID)
}

// chatScripted sends one turn with its scripted answers. It fails unless every step is used by this turn, and
// returns the request bodies the model saw during the turn with the reply.
func (f *chatFixture) chatScripted(message string, steps []packChatStep) ([]string, string) {
	f.t.Helper()
	f.model.script(steps)
	before := f.model.served()
	code, raw := f.post(message)
	if code != http.StatusOK {
		f.t.Fatalf("chat %q = %d: %s", message, code, raw)
	}
	if got := f.model.served() - before; got != len(steps) {
		f.t.Fatalf("chat %q: model answered %d times, want %d", message, got, len(steps))
	}
	f.model.mu.Lock()
	bodies := append([]string(nil), f.model.bodies[before:]...)
	f.model.mu.Unlock()
	return bodies, chatReply(raw)
}

// chatReply returns the reply text of a chat response body, or "" when it has none.
func chatReply(raw []byte) string {
	var out struct {
		Data struct {
			Reply string `json:"reply"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.Data.Reply
}

// callStep is a scripted tool call with fixed arguments.
func callStep(tool string, args any) packChatStep {
	raw, _ := json.Marshal(args)
	return packChatStep{tool: tool, args: func() string { return string(raw) }}
}

// confirmStep is a scripted confirm_write for the open preview whose summary contains needle. It reads the
// write_id from the request the model is answering, where the open previews are listed.
func confirmStep(f *chatFixture, needle string) packChatStep {
	return packChatStep{tool: "confirm_write", args: func() string {
		raw, _ := json.Marshal(map[string]any{"write_id": writeIDFor(f.model.lastBody(), needle)})
		return string(raw)
	}}
}

// textStep is a scripted final answer.
func textStep(content string) packChatStep {
	return packChatStep{content: content}
}
