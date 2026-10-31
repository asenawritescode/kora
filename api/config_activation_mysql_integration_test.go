//go:build integration

package api

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/configstore"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/site"
	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
)

// TestMySQLConfigActivationJournalRetry is opt-in and uses an isolated schema
// on a loopback disposable MySQL service.
func TestMySQLConfigActivationJournalRetry(t *testing.T) {
	if os.Getenv("KORA_TEST_ACTIVATION_CRASH_CHILD") == "1" {
		runActivationCrashChild(t)
		return
	}
	dsn := strings.TrimSpace(os.Getenv("KORA_CONFIG_DISPOSABLE_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_CONFIG_DISPOSABLE_MYSQL_DSN to a disposable loopback MySQL service")
	}
	if !strings.Contains(dsn, "tcp(127.0.0.1:") && !strings.Contains(dsn, "tcp(localhost:") {
		t.Fatalf("refusing non-loopback MySQL DSN for destructive integration test")
	}
	rootConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse disposable MySQL DSN:", err)
	}
	rootConfig.DBName = ""
	root, err := sql.Open("mysql", rootConfig.FormatDSN())
	if err != nil {
		t.Fatal("open disposable MySQL server:", err)
	}
	if err := root.Ping(); err != nil {
		root.Close()
		t.Fatal("ping disposable MySQL server:", err)
	}
	databaseName := fmt.Sprintf("kora_activate_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + databaseName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		root.Close()
		t.Fatal("create isolated database:", err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec("DROP DATABASE IF EXISTS `" + databaseName + "`")
		_ = root.Close()
	})
	tenantConfig := *rootConfig
	tenantConfig.DBName = databaseName
	database, err := sql.Open("mysql", tenantConfig.FormatDSN())
	if err != nil {
		t.Fatal("open isolated activation database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		t.Fatal("ping isolated activation database:", err)
	}
	dialect := kdb.Resolve("mysql")
	if err := site.BootstrapSystemTables(database, dialect); err != nil {
		t.Fatal("bootstrap isolated activation database:", err)
	}
	oldType := &doctype.DocType{Name: "RecoveryProbe", Module: "Test", Fields: []doctype.Field{{Fieldname: "subject", Fieldtype: "Data"}}}
	targetType := &doctype.DocType{Name: "RecoveryProbe", Module: "Test", Fields: []doctype.Field{{Fieldname: "subject", Fieldtype: "Data"}, {Fieldname: "notes", Fieldtype: "Small Text"}, {Fieldname: "summary", Fieldtype: "Small Text"}}}
	oldSnapshot := &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{oldType}}
	targetSnapshot := &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{targetType}}
	for _, statement := range dialect.CreateTable(oldType) {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal("create pre-activation doctype table:", err)
		}
	}
	if err := configstore.NewStore(database, dialect).SaveDocType(oldType, "site-mysql"); err != nil {
		t.Fatal("save pre-activation doctype fixture:", err)
	}
	readConfigRevision := func() uint64 {
		t.Helper()
		revision, err := configstore.CurrentConfigRevision(context.Background(), database, dialect)
		if err != nil {
			t.Fatal("read tenant config revision:", err)
		}
		return revision
	}
	if got := readConfigRevision(); got != 2 {
		t.Fatalf("revision after bootstrap plus initial active DocType = %d, want 2", got)
	}
	oldConfig := doctype.ToSExpr(oldSnapshot)
	targetConfig := doctype.ToSExpr(targetSnapshot)
	targetSnapshot, err = doctype.ParseConfig(targetConfig)
	if err != nil {
		t.Fatal("parse persisted activation snapshot fixture:", err)
	}
	_, err = database.Exec(`INSERT INTO _kora_config_version
		(id, site, version, created_at, created_by, label, status, is_active, config)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?), (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"active-mysql", "site-mysql", 1, time.Now(), "tester", "Active", "Active", 1, oldConfig,
		"draft-mysql", "site-mysql", 2, time.Now(), "tester", "Draft", "Draft", 0, targetConfig)
	if err != nil {
		t.Fatal("insert activation version fixtures:", err)
	}
	if _, err := database.Exec(`CREATE TRIGGER reject_activation_history BEFORE INSERT ON _kora_config_version
		FOR EACH ROW BEGIN
		IF NEW.status = 'Active' AND NEW.label LIKE 'Activated version %' THEN
		SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'intentional activation failure';
		END IF;
		END`); err != nil {
		t.Fatal("create activation failure trigger:", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal("close setup connection before crash child:", err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestMySQLConfigActivationJournalRetry$", "-test.v")
	child.Env = append(os.Environ(),
		"KORA_TEST_ACTIVATION_CRASH_CHILD=1",
		"KORA_TEST_ACTIVATION_CRASH_DSN="+tenantConfig.FormatDSN(),
	)
	childOutput, childErr := child.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(childErr, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf("activation child exit = %v, want forced process exit 86; output: %s", childErr, childOutput)
	}
	database, err = sql.Open("mysql", tenantConfig.FormatDSN())
	if err != nil {
		t.Fatal("reopen isolated database after simulated process loss:", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if err := database.Ping(); err != nil {
		t.Fatal("ping reopened database:", err)
	}
	registry := doctype.NewRegistry()
	registry.Register(oldType)
	var partialColumnCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name IN ('notes', 'summary')`, oldType.RawTableName()).Scan(&partialColumnCount); err != nil {
		t.Fatal("inspect partial schema after hard process exit:", err)
	}
	if partialColumnCount != 1 {
		t.Fatalf("partial schema after process exit has %d new columns, want one; child output: %s", partialColumnCount, childOutput)
	}
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: kdb.Resolve("mysql")})
	activate := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		gin.SetMode(gin.TestMode)
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/system/config/versions/draft-mysql/activate", nil)
		ctx.Params = gin.Params{{Key: "id", Value: "draft-mysql"}}
		ctx.Set("site_db", database)
		ctx.Set("site_registry", registry)
		ctx.Set("site_name", "site-mysql")
		ctx.Set("site_db_type", "mysql")
		ctx.Set("user", "tester")
		ctx.Set("user_roles", []string{"Administrator"})
		handler.HandleConfigVersionActivate(ctx)
		return recorder
	}
	rollback := func(targetID, key string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		gin.SetMode(gin.TestMode)
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/system/config/versions/"+targetID+"/rollback", nil)
		ctx.Request.Header.Set("Idempotency-Key", key)
		ctx.Params = gin.Params{{Key: "id", Value: targetID}}
		ctx.Set("site_db", database)
		ctx.Set("site_registry", registry)
		ctx.Set("site_name", "site-mysql")
		ctx.Set("site_db_type", "mysql")
		ctx.Set("user", "tester")
		ctx.Set("user_roles", []string{"Administrator"})
		handler.HandleConfigVersionRollback(ctx)
		return recorder
	}
	first := activate()
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("activation with injected version-write failure returned HTTP %d: %s", first.Code, first.Body.String())
	}
	if got := readConfigRevision(); got != 2 {
		t.Fatalf("failed activation advanced tenant config revision to %d, want 2", got)
	}
	var phase string
	if err := database.QueryRow(`SELECT phase FROM _kora_config_operation WHERE site = ? AND version_id = ? AND kind = 'activate'`, "site-mysql", "draft-mysql").Scan(&phase); err != nil {
		t.Fatal("read failed activation journal:", err)
	}
	if phase != configstore.ActivationValidated {
		t.Fatalf("failed activation journal phase = %q, want validated", phase)
	}
	var addedColumnCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name IN ('notes', 'summary')`, oldType.RawTableName()).Scan(&addedColumnCount); err != nil {
		t.Fatal("verify non-transactional DDL survived failed activation:", err)
	}
	if addedColumnCount != 2 {
		t.Fatalf("recovered DDL column count = %d, want both columns", addedColumnCount)
	}
	if _, err := database.Exec(`DROP TRIGGER reject_activation_history`); err != nil {
		t.Fatal("remove activation failure trigger:", err)
	}
	second := activate()
	if second.Code != http.StatusOK {
		t.Fatalf("activation retry returned HTTP %d: %s", second.Code, second.Body.String())
	}
	if got := readConfigRevision(); got != 3 {
		t.Fatalf("successful activation revision = %d, want 3", got)
	}
	if err := database.QueryRow(`SELECT phase FROM _kora_config_operation WHERE site = ? AND version_id = ? AND kind = 'activate'`, "site-mysql", "draft-mysql").Scan(&phase); err != nil {
		t.Fatal("read recovered activation journal:", err)
	}
	if phase != configstore.ActivationCompleted {
		t.Fatalf("recovered activation journal phase = %q, want completed", phase)
	}
	if _, err := database.Exec("INSERT INTO `"+oldType.RawTableName()+"` (name, subject, notes) VALUES (?, ?, ?)", "invoice-1", "Invoice", "preserve on rollback"); err != nil {
		t.Fatal("insert live record before rollback:", err)
	}
	if _, err := database.Exec(`CREATE TRIGGER reject_rollback_history BEFORE INSERT ON _kora_config_version
		FOR EACH ROW BEGIN
		IF NEW.status = 'Active' AND NEW.label = 'Rollback to version active-mysql' THEN
		SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'intentional rollback failure';
		END IF;
		END`); err != nil {
		t.Fatal("create rollback failure trigger:", err)
	}
	firstRollback := rollback("active-mysql", "rollback-op-1")
	if firstRollback.Code != http.StatusInternalServerError {
		t.Fatalf("rollback with injected version-write failure returned HTTP %d: %s", firstRollback.Code, firstRollback.Body.String())
	}
	if got := readConfigRevision(); got != 3 {
		t.Fatalf("failed rollback advanced tenant config revision to %d, want 3", got)
	}
	if err := database.QueryRow(`SELECT phase FROM _kora_config_operation WHERE site = ? AND version_id = ? AND kind = 'rollback'`, "site-mysql", restoreOperationID("site-mysql", "active-mysql", "rollback-op-1")).Scan(&phase); err != nil {
		t.Fatal("read failed rollback journal:", err)
	}
	if phase != configstore.ActivationValidated {
		t.Fatalf("failed rollback journal phase = %q, want validated", phase)
	}
	if _, err := database.Exec(`DROP TRIGGER reject_rollback_history`); err != nil {
		t.Fatal("remove rollback failure trigger:", err)
	}
	retriedRollback := rollback("active-mysql", "rollback-op-1")
	if retriedRollback.Code != http.StatusOK {
		t.Fatalf("rollback retry returned HTTP %d: %s", retriedRollback.Code, retriedRollback.Body.String())
	}
	if got := readConfigRevision(); got != 4 {
		t.Fatalf("successful rollback revision = %d, want 4", got)
	}
	columns, err := database.Query(`SELECT column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ?`, oldType.RawTableName())
	if err != nil {
		t.Fatal("list columns after rollback:", err)
	}
	var quarantinedNotes string
	for columns.Next() {
		var column string
		if err := columns.Scan(&column); err != nil {
			columns.Close()
			t.Fatal("scan rollback column:", err)
		}
		if strings.HasPrefix(column, "_dropquar_notes_") {
			quarantinedNotes = column
		}
	}
	if err := columns.Err(); err != nil {
		columns.Close()
		t.Fatal("iterate rollback columns:", err)
	}
	columns.Close()
	if quarantinedNotes == "" {
		t.Fatal("rollback did not quarantine the removed notes column")
	}
	var preserved string
	if err := database.QueryRow("SELECT "+dialect.QuoteIdent(quarantinedNotes)+" FROM `"+oldType.RawTableName()+"` WHERE name = ?", "invoice-1").Scan(&preserved); err != nil {
		t.Fatal("read quarantined notes value:", err)
	}
	if preserved != "preserve on rollback" {
		t.Fatalf("quarantined notes = %q, want preserved value", preserved)
	}
	restoreNotes := rollback("draft-mysql", "rollback-op-2")
	if restoreNotes.Code != http.StatusOK {
		t.Fatalf("restore previously quarantined field returned HTTP %d: %s", restoreNotes.Code, restoreNotes.Body.String())
	}
	if err := database.QueryRow("SELECT notes FROM `"+oldType.RawTableName()+"` WHERE name = ?", "invoice-1").Scan(&preserved); err != nil {
		t.Fatal("read restored notes value:", err)
	}
	if preserved != "preserve on rollback" {
		t.Fatalf("restored notes = %q, want original quarantined value", preserved)
	}

	// The management activation path uses a compare-and-set in the same
	// transaction as authorization/config writes. A stale revision must roll
	// back both the config row and its epoch updates.
	currentRevision := readConfigRevision()
	casStore := configstore.NewStore(database, dialect)
	activateAtRevision := func(role string, expected uint64) error {
		tx, txErr := database.BeginTx(context.Background(), nil)
		if txErr != nil {
			return txErr
		}
		defer tx.Rollback()
		snapshot := &doctype.ConfigSnapshot{Roles: []*doctype.Role{{Name: role, WorkspaceAccess: true}}}
		if txErr = casStore.ActivateSnapshotAtConfigRevision(tx, snapshot, "site-mysql", dialect, expected); txErr != nil {
			return txErr
		}
		return tx.Commit()
	}
	if err := activateAtRevision("Management CAS Role", currentRevision); err != nil {
		t.Fatal("activate at current MySQL tenant revision:", err)
	}
	if err := activateAtRevision("Stale Management CAS Role", currentRevision); !errors.Is(err, configstore.ErrConfigRevisionConflict) {
		t.Fatalf("stale MySQL activation error = %v, want revision conflict", err)
	}
	if got := readConfigRevision(); got != currentRevision+1 {
		t.Fatalf("MySQL revision after current/stale activations = %d, want %d", got, currentRevision+1)
	}
	var staleRoleCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_role WHERE name = ?`, "Stale Management CAS Role").Scan(&staleRoleCount); err != nil {
		t.Fatal("check stale MySQL role rollback:", err)
	}
	if staleRoleCount != 0 {
		t.Fatal("stale MySQL activation persisted a role")
	}
}

func runActivationCrashChild(t *testing.T) {
	dsn := os.Getenv("KORA_TEST_ACTIVATION_CRASH_DSN")
	if dsn == "" {
		t.Fatal("crash child is missing its disposable MySQL DSN")
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse child MySQL DSN")
	}
	base, err := mysql.NewConnector(config)
	if err != nil {
		t.Fatal("create child MySQL connector:", err)
	}
	database := sql.OpenDB(ddlCrashConnector{base: base})
	database.SetMaxOpenConns(1)
	defer database.Close()
	if err := database.Ping(); err != nil {
		t.Fatal("ping child MySQL database:", err)
	}
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "RecoveryProbe", Module: "Test", Fields: []doctype.Field{{Fieldname: "subject", Fieldtype: "Data"}}})
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: kdb.Resolve("mysql")})
	recorder := httptest.NewRecorder()
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/system/config/versions/draft-mysql/activate", nil)
	ctx.Params = gin.Params{{Key: "id", Value: "draft-mysql"}}
	ctx.Set("site_db", database)
	ctx.Set("site_registry", registry)
	ctx.Set("site_name", "site-mysql")
	ctx.Set("site_db_type", "mysql")
	ctx.Set("user", "tester")
	ctx.Set("user_roles", []string{"Administrator"})
	handler.HandleConfigVersionActivate(ctx)
	t.Fatalf("activation returned HTTP %d before the DDL crash point: %s", recorder.Code, recorder.Body.String())
}

type ddlCrashConnector struct{ base driver.Connector }

func (c ddlCrashConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return ddlCrashConn{Conn: conn}, nil
}

func (c ddlCrashConnector) Driver() driver.Driver { return c.base.Driver() }

type ddlCrashConn struct{ driver.Conn }

func (c ddlCrashConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	result, err := execer.ExecContext(ctx, query, args)
	if err == nil && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "ALTER TABLE") {
		os.Exit(86)
	}
	return result, err
}
