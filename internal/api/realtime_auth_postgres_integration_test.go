//go:build integration

package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

func TestPostgresRealtimeReplayAndNATSUseCurrentOwner(t *testing.T) {
	dsn := os.Getenv("KORA_API_DISPOSABLE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_DISPOSABLE_POSTGRES_DSN to a disposable PostgreSQL service")
	}
	root, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL service:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Fatal("ping disposable PostgreSQL service:", err)
	}
	schemaName := fmt.Sprintf("kora_realtime_%x", time.Now().UnixNano())
	if _, err := root.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		_ = root.Close()
		t.Fatal("create isolated realtime schema:", err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`)
		_ = root.Close()
	})
	parsedDSN, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("parse disposable PostgreSQL DSN:", err)
	}
	query := parsedDSN.Query()
	query.Set("search_path", schemaName)
	parsedDSN.RawQuery = query.Encode()
	database, err := sql.Open("postgres", parsedDSN.String())
	if err != nil {
		t.Fatal("open isolated realtime schema:", err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Ping(); err != nil {
		t.Fatal("ping isolated realtime schema:", err)
	}

	dialect := kdb.Resolve("postgres")
	dt := &doctype.DocType{Name: "Invoice", Fields: []doctype.Field{
		{Fieldname: "amount", Fieldtype: "Currency"},
		{Fieldname: "secret", Fieldtype: "Data", Hidden: true},
	}}
	for _, statement := range dialect.CreateTable(dt) {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal("create realtime document table:", err)
		}
	}
	if _, err := database.Exec(`CREATE TABLE _kora_outbox (
		id TEXT PRIMARY KEY, event_type TEXT NOT NULL, site TEXT NOT NULL,
		aggregate_type TEXT NOT NULL, aggregate_id TEXT NOT NULL, payload TEXT, created_at TIMESTAMP NOT NULL
	)`); err != nil {
		t.Fatal("create realtime replay fixture:", err)
	}
	if _, err := database.Exec(kdb.Rebind(dialect, "INSERT INTO "+dialect.QuoteIdent(dt.RawTableName())+" (name, owner, amount, secret) VALUES (?, ?, ?, ?)"), "INV-1", "alice@example.test", 25, "hidden"); err != nil {
		t.Fatal("insert live invoice:", err)
	}
	if _, err := database.Exec(kdb.Rebind(dialect, `INSERT INTO _kora_outbox (id, event_type, site, aggregate_type, aggregate_id, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`), "evt-2", "kora.Invoice.after_update", "tenant-a", "Invoice", "INV-1", `{}`, time.Now().UTC()); err != nil {
		t.Fatal("insert replay event:", err)
	}

	registry := doctype.NewRegistry()
	registry.Register(dt)
	registry.Permissions.AddRole(&doctype.Role{Name: "InvoiceOwner"})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Invoice", Role: "InvoiceOwner", Read: true, IfOwner: true})
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: dialect})
	gin.SetMode(gin.TestMode)
	contextFor := func(user string) *gin.Context {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodGet, "/api/system/realtime?after=evt-1", nil)
		ctx.Set("site_db", database)
		ctx.Set("site_registry", registry)
		ctx.Set("site_name", "tenant-a")
		execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: user, PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{"InvoiceOwner"}, AuthenticatedAt: time.Now().UTC()})
		if err != nil {
			t.Fatal("create verified realtime actor:", err)
		}
		ctx.Set("policy_execution", execution)
		return ctx
	}

	var replayed int
	if err := handler.replayRealtime(contextFor("alice@example.test"), "evt-1", nil, func(payload []byte) error {
		var message map[string]any
		if err := json.Unmarshal(payload, &message); err != nil {
			return err
		}
		if message["type"] == "change" {
			replayed++
		}
		return nil
	}); err != nil {
		t.Fatal("replay for current owner:", err)
	}
	if replayed != 1 {
		t.Fatalf("current owner replayed %d changes, want 1", replayed)
	}

	if _, err := database.Exec(kdb.Rebind(dialect, "UPDATE "+dialect.QuoteIdent(dt.RawTableName())+" SET owner = ? WHERE name = ?"), "bob@example.test", "INV-1"); err != nil {
		t.Fatal("transfer invoice owner:", err)
	}
	raw := []byte(`{"id":"evt-2","operation":"update","aggregate_type":"Invoice","aggregate_id":"INV-1","data":{"owner":"alice@example.test","amount":25,"secret":"hidden","old_data":{"amount":1}}}`)
	if _, allowed := handler.authorizeRealtimeEnvelope(contextFor("alice@example.test"), raw); allowed {
		t.Fatal("NATS delivered delayed event to former owner")
	}
	if _, allowed := handler.authorizeRealtimeEnvelope(contextFor("bob@example.test"), raw); !allowed {
		t.Fatal("NATS denied event to current owner")
	}
	replayed = 0
	if err := handler.replayRealtime(contextFor("alice@example.test"), "evt-1", nil, func(payload []byte) error {
		var message map[string]any
		if err := json.Unmarshal(payload, &message); err != nil {
			return err
		}
		if message["type"] == "change" {
			replayed++
		}
		return nil
	}); err != nil {
		t.Fatal("replay after ownership change:", err)
	}
	if replayed != 0 {
		t.Fatalf("former owner replayed %d changes after transfer, want 0", replayed)
	}
	projected, ok := handler.authorizeRealtimeEnvelope(contextFor("bob@example.test"), raw)
	if !ok {
		t.Fatal("current owner NATS envelope was rejected")
	}
	var sanitized map[string]any
	if err := json.Unmarshal(projected, &sanitized); err != nil {
		t.Fatal("decode sanitized envelope:", err)
	}
	data := sanitized["data"].(map[string]any)
	if _, found := data["secret"]; found {
		t.Fatal("hidden field escaped realtime projection")
	}
	if _, found := data["old_data"]; found {
		t.Fatal("historical values escaped realtime projection")
	}
}
