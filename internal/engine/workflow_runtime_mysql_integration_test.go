//go:build integration

package engine

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/outbox"
	requestsql "github.com/asenawritescode/kora/internal/requests/sqlstore"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/go-sql-driver/mysql"
)

func TestSelfHostedWorkflowConfigAssemblesTenantRuntimeOnMySQL(t *testing.T) {
	dsn := os.Getenv("KORA_ENGINE_WORKFLOW_DISPOSABLE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_ENGINE_WORKFLOW_DISPOSABLE_MYSQL_DSN to an isolated disposable MySQL service")
	}
	rootConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse disposable MySQL DSN:", err)
	}
	rootConfig.DBName = ""
	root, err := sql.Open("mysql", rootConfig.FormatDSN())
	if err != nil {
		t.Fatal("open disposable MySQL service:", err)
	}
	if err := root.PingContext(t.Context()); err != nil {
		_ = root.Close()
		t.Fatal("ping disposable MySQL service:", err)
	}
	databaseName := fmt.Sprintf("kora_engine_workflow_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.ExecContext(t.Context(), "CREATE DATABASE `"+databaseName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create isolated disposable database:", err)
	}
	t.Cleanup(func() {
		_, _ = root.ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+databaseName+"`")
		_ = root.Close()
	})
	rootConfig.DBName = databaseName
	database, err := sql.Open("mysql", rootConfig.FormatDSN())
	if err != nil {
		t.Fatal("open isolated disposable database:", err)
	}
	database.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = database.Close() })
	dialect := db.Resolve("mysql")
	if err := database.PingContext(t.Context()); err != nil {
		t.Fatal("ping isolated disposable database:", err)
	}
	if err = site.BootstrapSystemTables(context.Background(), database, dialect); err != nil {
		t.Fatal("bootstrap Engine tables:", err)
	}
	requestDDL, err := requestsql.SchemaStatements(dialect)
	if err != nil {
		t.Fatal("build request schema:", err)
	}
	for _, statement := range requestDDL {
		if _, err := database.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("create request schema: %v\n%s", err, statement)
		}
	}
	registry := doctype.NewRegistry()
	registry.LoadFull(nil, nil, nil)
	loaded := &knet.LoadedSite{SiteID: "tenant-" + contract.NewID(), Name: "workflow-selfhost.example.test", DB: database, Registry: registry}
	requestService, _, err := composeRequests(database, dialect, registry, nil)
	if err != nil {
		t.Fatal("compose request service:", err)
	}
	admission, err := dbaccess.NewAdmission(2, 2, time.Second)
	if err != nil {
		t.Fatal("create workflow active-work admission:", err)
	}
	worker, definitions, instances, err := configureTenantWorkflow(t.Context(), SelfHostedWorkflowConfig(), loaded, tenantWorkflowDeps{
		dialect: dialect, requests: requestService, admission: admission, domainKernel: kernel.New(dialect, outbox.NewSQLWriter(dialect)),
	})
	if err != nil {
		t.Fatal("assemble self-hosted tenant workflow:", err)
	}
	if worker == nil || definitions == nil || instances == nil {
		t.Fatalf("self-hosted workflow services missing: worker=%t definitions=%t instances=%t", worker != nil, definitions != nil, instances != nil)
	}
	if err := worker.RunOnce(t.Context()); err != nil {
		t.Fatalf("run bounded workflow recovery pass on empty tenant: %v", err)
	}
}
