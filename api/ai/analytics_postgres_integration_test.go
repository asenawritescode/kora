package ai

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/policy"
	_ "github.com/lib/pq"
)

func TestLivePostgresAnalyticsInsights(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_POSTGRES_DSN to test AI analytics queries against a disposable PostgreSQL database")
	}
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable PostgreSQL database:", err)
	}
	schemaName := fmt.Sprintf("kora_ai_analytics_%d", time.Now().UnixNano())
	if _, err := database.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		t.Fatal("create isolated PostgreSQL schema:", err)
	}
	defer func() { _, _ = database.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`) }()
	if _, err := database.Exec(`SET search_path TO "` + schemaName + `"`); err != nil {
		t.Fatal("select isolated PostgreSQL schema:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_analytics_daily (site TEXT NOT NULL, doctype TEXT NOT NULL, metric TEXT NOT NULL, dimension TEXT NOT NULL, date DATE NOT NULL, value DOUBLE PRECISION NOT NULL)`); err != nil {
		t.Fatal("create analytics daily fixture:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value) VALUES ($1,$2,$3,$4,CURRENT_DATE,$5)`, "site-pg", "Sale", "sale_count", "", 3.0); err != nil {
		t.Fatal("insert analytics rollup fixture:", err)
	}
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Sale"})
	registry.Permissions.AddRole(&doctype.Role{Name: doctype.AdminRole})
	actor := contract.ActorContext{PrincipalID: "analytics-admin", PrincipalType: contract.PrincipalHuman, Site: "site-pg", Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now().UTC()}
	execution, err := policy.NewVerifiedExecution("site-pg", actor)
	if err != nil {
		t.Fatal("create analytics execution:", err)
	}
	tx := &orm.TxManager{DB: database, Registry: registry, Dialect: kdb.Resolve("postgres"), Execution: execution}
	all := executeAnalyticsInsights(tx, registry, "all", "site-pg", execution)
	if !strings.Contains(all, "Sale") {
		t.Fatalf("PostgreSQL analytics doctype discovery = %q, want Sale", all)
	}
	insights := executeAnalyticsInsights(tx, registry, "Sale", "site-pg", execution)
	if !strings.Contains(insights, "Total: 3") {
		t.Fatalf("PostgreSQL analytics insights = %q, want total 3", insights)
	}
}
