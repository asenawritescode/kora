//go:build integration

package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/auth"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/envcompat"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
)

// A WhatsApp turn runs the workspace chat as the session's subject user, through the real SiteGuard. The scripted
// model previews an Item with create_record; the person's yes confirms it, and the Item is owned by the subject user.
// A session without a subject user, and a session for a disabled user, are refused.
const (
	channelChatSubject  = "shop-owner"
	channelChatDisabled = "shop-former"
	channelChatClient   = "kora-cloud-channel"
)

func TestChannelChatSubjectUserAcrossDialects(t *testing.T) {
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
			siteName := fmt.Sprintf("channel-chat-%d.example.test", time.Now().UnixNano())
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
			if err = install(info, loaded); err != nil {
				t.Fatal(err)
			}
			services := *loaded.RuntimeServices
			itemTable := dialect.QuoteIdent(loaded.Registry.Get("Item").RawTableName())
			ownedItems := func(owner string) string {
				t.Helper()
				var n string
				if qerr := database.QueryRow(db.Rebind(dialect, "SELECT COUNT(*) FROM "+itemTable+" WHERE owner = ?"), owner).Scan(&n); qerr != nil {
					t.Fatal("count items:", qerr)
				}
				return n
			}
			for _, user := range []struct {
				name    string
				enabled bool
			}{{channelChatSubject, true}, {channelChatDisabled, false}} {
				_, seedErr := database.Exec(db.Rebind(dialect, `INSERT INTO _kora_user (name, site, email, password_hash, full_name, enabled, roles) VALUES (?,?,?,?,?,?,?)`),
					user.name, siteName, user.name+"@channel.example.test", "", user.name, db.BoolParam(dialect, user.enabled), doctype.AdminRole)
				if seedErr != nil {
					t.Fatal("seed user:", seedErr)
				}
			}

			model := startPackChatModel(t)
			t.Setenv("KORA_CLOUD_AI_GATEWAY_URL", model.server.URL)
			t.Setenv("KORA_CLOUD_AI_GATEWAY_MODEL", "")
			t.Setenv(envcompat.IntegrationTokenEnv, "channel-chat-token")

			handler := api.NewHandler(loaded.Registry, nil)
			setSite := func(c *gin.Context) {
				c.Set("site_name", siteName)
				c.Set("site_id", siteName)
				c.Set("site_db", database)
				c.Set("site_db_type", tc.driver)
				c.Set("site_registry", loaded.Registry)
				c.Set("site_runtime_services", services)
			}
			// Issuing is Cloud's extension call. Its bearer check is stubbed here: on Postgres the extension token lookup
			// compares a boolean column with 1 and never authenticates, so the channel route, which runs through the
			// real SiteGuard, is the part under test.
			asCloudExtension := func(c *gin.Context) {
				c.Set("auth_type", "extension")
				c.Set("extension_name", channelChatClient)
				c.Next()
			}
			guard := &auth.SiteGuard{}
			router := gin.New()
			router.Use(setSite)
			router.POST("/api/internal/channel/sessions/issue", asCloudExtension, handler.HandleChannelSessionIssue)
			apiGroup := router.Group("/api", guard.Middleware(true))
			apiGroup.POST("/internal/channel/chat", handler.HandleChannelChat)
			do := func(method, path, bearer string, body any) *httptest.ResponseRecorder {
				t.Helper()
				raw, _ := json.Marshal(body)
				out := httptest.NewRecorder()
				req := httptest.NewRequest(method, path, bytes.NewReader(raw))
				if bearer != "" {
					req.Header.Set("Authorization", "Bearer "+bearer)
				}
				router.ServeHTTP(out, req)
				return out
			}
			issue := func(subject string) string {
				t.Helper()
				out := do(http.MethodPost, "/api/internal/channel/sessions/issue", "", map[string]any{
					"client_name": channelChatClient, "conversation_key": "wa-thread-1", "provider": "twilio_whatsapp",
					"sender_address": "+254700000001", "subject_user": subject, "permissions": []any{},
					"trusted_until": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				})
				if subject == channelChatDisabled {
					if out.Code != http.StatusForbidden {
						t.Fatalf("issue for disabled user = %d: %s", out.Code, out.Body)
					}
					return ""
				}
				var session struct {
					Data struct {
						AccessToken string `json:"access_token"`
					} `json:"data"`
				}
				if out.Code != http.StatusCreated || json.Unmarshal(out.Body.Bytes(), &session) != nil || session.Data.AccessToken == "" {
					t.Fatalf("issue for %q = %d: %s", subject, out.Code, out.Body)
				}
				return session.Data.AccessToken
			}
			type chatReply struct {
				Data struct {
					Result               string `json:"result"`
					AwaitingConfirmation bool   `json:"awaiting_confirmation"`
					Usage                struct {
						InputTokens       int64 `json:"input_tokens"`
						CachedInputTokens int64 `json:"cached_input_tokens"`
						OutputTokens      int64 `json:"output_tokens"`
					} `json:"usage"`
				} `json:"data"`
			}
			chat := func(bearer, message string, steps []packChatStep) (*httptest.ResponseRecorder, chatReply) {
				t.Helper()
				model.script(steps)
				before := model.served()
				out := do(http.MethodPost, "/api/internal/channel/chat", bearer, map[string]any{"message": message, "conversation_key": "wa-thread-1"})
				var reply chatReply
				_ = json.Unmarshal(out.Body.Bytes(), &reply)
				if out.Code == http.StatusOK && model.served()-before != len(steps) {
					t.Fatalf("chat %q: model answered %d times, want %d", message, model.served()-before, len(steps))
				}
				return out, reply
			}

			// A session without a subject user cannot use the chat route.
			delegated := do(http.MethodPost, "/api/internal/channel/sessions/issue", "", map[string]any{
				"client_name": channelChatClient, "conversation_key": "wa-delegated", "provider": "twilio_whatsapp",
				"sender_address": "+254700000002", "permissions": []any{},
				"trusted_until": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
			if delegated.Code != http.StatusCreated {
				t.Fatalf("issue delegated session = %d: %s", delegated.Code, delegated.Body)
			}
			var delegatedSession struct {
				Data struct {
					AccessToken string `json:"access_token"`
				} `json:"data"`
			}
			_ = json.Unmarshal(delegated.Body.Bytes(), &delegatedSession)
			if out, _ := chat(delegatedSession.Data.AccessToken, "Add item Tea.", nil); out.Code != http.StatusForbidden {
				t.Fatalf("chat on a session without subject_user = %d, want 403", out.Code)
			}
			issue(channelChatDisabled)

			session := issue(channelChatSubject)
			before := ownedItems(channelChatSubject)
			beforeCount, err := strconv.Atoi(before)
			if err != nil {
				t.Fatal("parse item count:", err)
			}
			// Turn 1: the model previews the Item and asks. Nothing is saved yet.
			out, reply := chat(session, "Add an item Tea at 50.", []packChatStep{
				{tool: "create_record", args: func() string {
					return `{"record_type":"Item","data":{"item_name":"Tea","uom":"Nos","price":50,"cost":30}}`
				}},
				{content: "Save Tea at 50 KES? Reply yes."},
			})
			if out.Code != http.StatusOK {
				t.Fatalf("turn 1 = %d: %s", out.Code, out.Body)
			}
			if !reply.Data.AwaitingConfirmation {
				t.Fatal("turn 1 left a preview, but awaiting_confirmation is false")
			}
			if !strings.Contains(model.bodies[0], "REPLY STYLE") {
				t.Fatal("the WhatsApp reply style is missing from the turn context")
			}
			if n := ownedItems(channelChatSubject); n != before {
				t.Fatalf("items owned by the subject before confirmation = %s, want %s", n, before)
			}
			if reply.Data.Usage.InputTokens != 20 || reply.Data.Usage.OutputTokens != 20 {
				t.Fatalf("turn 1 usage = %+v, want the two scripted calls summed (20 input, 20 output)", reply.Data.Usage)
			}

			// Turn 2: after yes, the model confirms the preview by its write_id, and the Item is saved.
			var writeID string
			out, reply = chat(session, "yes", []packChatStep{
				{tool: "confirm_write", args: func() string {
					writeID = writeIDIn(model.bodies[len(model.bodies)-1])
					return fmt.Sprintf(`{"write_id":%q}`, writeID)
				}},
				{content: "Tea is saved."},
			})
			if out.Code != http.StatusOK || writeID == "" {
				t.Fatalf("turn 2 = %d, write_id %q: %s", out.Code, writeID, out.Body)
			}
			if reply.Data.AwaitingConfirmation {
				t.Fatal("turn 2 confirmed the preview, but awaiting_confirmation is still true")
			}
			if n := ownedItems(channelChatSubject); n != fmt.Sprint(beforeCount+1) {
				t.Fatalf("items owned by the subject after confirm_write = %s, want %s + 1", n, before)
			}
			if n := ownedItems(channelChatDisabled); n != "0" {
				t.Fatalf("items owned by the disabled user = %s, want 0", n)
			}
		})
	}
}
