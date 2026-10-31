//go:build integration

package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
)

func TestMySQLRealtimeReplayAndNATSUseCurrentOwner(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_CONFIG_DISPOSABLE_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_CONFIG_DISPOSABLE_MYSQL_DSN to a disposable loopback MySQL service")
	}
	if !strings.Contains(dsn, "tcp(127.0.0.1:") && !strings.Contains(dsn, "tcp(localhost:") {
		t.Fatal("refusing non-loopback MySQL DSN for destructive integration test")
	}
	rootConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse disposable MySQL DSN:", err)
	}
	root, err := sql.Open("mysql", rootConfig.FormatDSN())
	if err != nil {
		t.Fatal("open disposable MySQL server:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Fatal("ping disposable MySQL server:", err)
	}
	databaseName := fmt.Sprintf("kora_realtime_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + databaseName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create isolated realtime database:", err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec("DROP DATABASE IF EXISTS `" + databaseName + "`")
		_ = root.Close()
	})
	tenantConfig := *rootConfig
	tenantConfig.DBName = databaseName
	database, err := sql.Open("mysql", tenantConfig.FormatDSN())
	if err != nil {
		t.Fatal("open isolated realtime database:", err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Ping(); err != nil {
		t.Fatal("ping isolated realtime database:", err)
	}

	dialect := kdb.Resolve("mysql")
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
		id VARCHAR(140) PRIMARY KEY, event_type VARCHAR(255) NOT NULL, site VARCHAR(140) NOT NULL,
		aggregate_type VARCHAR(140) NOT NULL, aggregate_id VARCHAR(140) NOT NULL, created_at DATETIME(6) NOT NULL
	) ENGINE=InnoDB`); err != nil {
		t.Fatal("create realtime replay fixture:", err)
	}
	if _, err := database.Exec("INSERT INTO `"+dt.RawTableName()+"` (name, owner, amount, secret) VALUES (?, ?, ?, ?)", "INV-1", "alice@example.test", 25, "hidden"); err != nil {
		t.Fatal("insert live invoice:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_outbox (id, event_type, site, aggregate_type, aggregate_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, "evt-2", "kora.Invoice.after_update", "tenant-a", "Invoice", "INV-1", time.Now().UTC()); err != nil {
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
		ctx.Set("user", user)
		ctx.Set("user_roles", []string{"InvoiceOwner"})
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

	// A delayed envelope still says Alice owned the record. Both the NATS
	// sanitizer and SQL replay must use the current row, now owned by Bob.
	if _, err := database.Exec("UPDATE `"+dt.RawTableName()+"` SET owner = ? WHERE name = ?", "bob@example.test", "INV-1"); err != nil {
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
