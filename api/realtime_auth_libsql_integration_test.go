//go:build integration

package api

import (
	"database/sql"
	"encoding/json"
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
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestLibSQLRealtimeReplayAndNATSUseCurrentOwner(t *testing.T) {
	dsn := os.Getenv("KORA_API_DISPOSABLE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_DISPOSABLE_LIBSQL_DSN to a disposable LibSQL database endpoint")
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable LibSQL endpoint:", err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable LibSQL endpoint:", err)
	}
	dialect := kdb.Resolve("libsql")
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	siteName := "realtime-" + suffix
	doctypeName := "Invoice" + suffix
	dt := &doctype.DocType{Name: doctypeName, Fields: []doctype.Field{
		{Fieldname: "amount", Fieldtype: "Currency"},
		{Fieldname: "secret", Fieldtype: "Data", Hidden: true},
	}}
	for _, statement := range dialect.CreateTable(dt) {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal("create realtime document table:", err)
		}
	}
	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS _kora_outbox (
		id TEXT PRIMARY KEY, event_type TEXT NOT NULL, site TEXT NOT NULL,
		aggregate_type TEXT NOT NULL, aggregate_id TEXT NOT NULL, created_at DATETIME NOT NULL
	)`); err != nil {
		t.Fatal("create realtime replay fixture:", err)
	}
	if _, err := database.Exec(kdb.Rebind(dialect, "INSERT INTO "+dialect.QuoteIdent(dt.RawTableName())+" (name, owner, amount, secret) VALUES (?, ?, ?, ?)"), "INV-1", "alice@example.test", 25, "hidden"); err != nil {
		t.Fatal("insert live invoice:", err)
	}
	eventID, cursor := "evt-"+suffix+"-2", "evt-"+suffix+"-1"
	if _, err := database.Exec(kdb.Rebind(dialect, `INSERT INTO _kora_outbox (id, event_type, site, aggregate_type, aggregate_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`), eventID, "kora."+doctypeName+".after_update", siteName, doctypeName, "INV-1", time.Now().UTC()); err != nil {
		t.Fatal("insert replay event:", err)
	}

	registry := doctype.NewRegistry()
	registry.Register(dt)
	registry.Permissions.AddRole(&doctype.Role{Name: "InvoiceOwner"})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: doctypeName, Role: "InvoiceOwner", Read: true, IfOwner: true})
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: dialect})
	gin.SetMode(gin.TestMode)
	contextFor := func(user string) *gin.Context {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodGet, "/api/system/realtime?after="+cursor, nil)
		ctx.Set("site_db", database)
		ctx.Set("site_registry", registry)
		ctx.Set("site_name", siteName)
		ctx.Set("user", user)
		ctx.Set("user_roles", []string{"InvoiceOwner"})
		return ctx
	}

	var replayed int
	if err := handler.replayRealtime(contextFor("alice@example.test"), cursor, nil, func(payload []byte) error {
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
	raw, err := json.Marshal(map[string]any{
		"id": eventID, "operation": "update", "aggregate_type": doctypeName, "aggregate_id": "INV-1",
		"data": map[string]any{"owner": "alice@example.test", "amount": 25, "secret": "hidden", "old_data": map[string]any{"amount": 1}},
	})
	if err != nil {
		t.Fatal("marshal delayed envelope:", err)
	}
	if _, allowed := handler.authorizeRealtimeEnvelope(contextFor("alice@example.test"), raw); allowed {
		t.Fatal("NATS delivered delayed event to former owner")
	}
	if _, allowed := handler.authorizeRealtimeEnvelope(contextFor("bob@example.test"), raw); !allowed {
		t.Fatal("NATS denied event to current owner")
	}
	replayed = 0
	if err := handler.replayRealtime(contextFor("alice@example.test"), cursor, nil, func(payload []byte) error {
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
		t.Fatal("current owner LibSQL envelope was rejected")
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
