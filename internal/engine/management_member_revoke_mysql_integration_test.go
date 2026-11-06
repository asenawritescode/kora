//go:build integration

package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/management"
	"github.com/asenawritescode/kora/management/sqlstore"
	"github.com/asenawritescode/kora/site"
	"github.com/go-sql-driver/mysql"
)

func newMySQLMemberFixture(t *testing.T) (*sql.DB, *auth.SessionManager, *managementMemberRevokeExecutor) {
	t.Helper()
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:kora123@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Skipf("MySQL unavailable: %v", err)
	}
	name := fmt.Sprintf("kora_member_revoke_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create disposable database:", err)
	}
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`"); _ = root.Close() })
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = name
	database, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	database.SetMaxOpenConns(8)
	dialect := db.Resolve("mysql")
	if err := site.BootstrapSystemTables(database, dialect); err != nil {
		t.Fatal("bootstrap system tables:", err)
	}
	sessions := auth.NewOwnedSessionManager(database).SetDialect(dialect)
	t.Cleanup(func() { _ = sessions.Close() })
	runtime := managementMemberRuntime{Site: memberTestSite, DB: database, Dialect: dialect, Sessions: sessions, Admission: memberTestAdmission{}}
	executor, err := newManagementMemberRevokeExecutor(func(context.Context, contract.TenantID) (managementMemberRuntime, func(), error) {
		return runtime, func() {}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return database, sessions, executor
}

func addMySQLUser(t *testing.T, database *sql.DB, name, roles string) {
	t.Helper()
	if _, err := database.Exec("INSERT INTO _kora_user (name, site, email, password_hash, full_name, enabled, roles) VALUES (?, ?, ?, '', ?, 1, ?)", name, memberTestSite, name+"@example.test", name, roles); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLRevokeMemberEndsAccessKeepsRecordsAndIsIdempotent(t *testing.T) {
	database, sessions, executor := newMySQLMemberFixture(t)
	ctx := context.Background()
	addMySQLUser(t, database, "cloud-admin-1", doctype.AdminRole)
	addMySQLUser(t, database, "cloud-cashier-1", "Cashier")
	if _, err := database.Exec("CREATE TABLE sale_note (name VARCHAR(64) PRIMARY KEY, owner VARCHAR(140) NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("INSERT INTO sale_note (name, owner) VALUES ('note-1', 'cloud-cashier-1')"); err != nil {
		t.Fatal(err)
	}
	sid, err := sessions.CreateSession(memberTestSite, &auth.User{Name: "cloud-cashier-1", Email: "cloud-cashier-1@example.test", Roles: []string{"Cashier"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sessions.GetSession(memberTestSite, sid); err != nil {
		t.Fatal("session must be valid before the revoke:", err)
	}
	for i := 0; i < 2; i++ {
		result, err := executor.Execute(ctx, revokeClaim(t, "cloud-cashier-1"))
		if err != nil || result.State != contract.OperationSucceeded {
			t.Fatalf("revoke run %d: %+v %v", i, result, err)
		}
	}
	if _, _, err := sessions.GetSession(memberTestSite, sid); err == nil {
		t.Fatal("old session still validates after the revoke")
	}
	if _, err := sessions.FindUserByCloudSubject(memberTestSite, "cloud-cashier-1"); !errors.Is(err, auth.ErrDisabledAccount) {
		t.Fatalf("handoff lookup = %v, want disabled account", err)
	}
	var owner, fullName string
	var roles sql.NullString
	if err := database.QueryRow("SELECT n.owner, u.full_name, u.roles FROM sale_note n JOIN _kora_user u ON u.name = n.owner AND u.site = ? WHERE n.name = 'note-1'", memberTestSite).Scan(&owner, &fullName, &roles); err != nil {
		t.Fatalf("a record owned by the revoked member must still resolve its owner: %v", err)
	}
	if owner != "cloud-cashier-1" || fullName != "cloud-cashier-1" || roles.String != "" {
		t.Fatalf("owner=%q full_name=%q roles=%q", owner, fullName, roles.String)
	}
}

func TestMySQLRevokeRefusesLastAdministratorAndSerializesConcurrentRevokes(t *testing.T) {
	database, _, executor := newMySQLMemberFixture(t)
	ctx := context.Background()
	addMySQLUser(t, database, "cloud-admin-1", doctype.AdminRole)
	result, err := executor.Execute(ctx, revokeClaim(t, "cloud-admin-1"))
	if err != nil || result.State != contract.OperationFailed || result.Error.Type != contract.CodeConflict {
		t.Fatalf("last administrator revoke = %+v %v, want conflict", result, err)
	}
	addMySQLUser(t, database, "cloud-admin-2", doctype.AdminRole)
	var wg sync.WaitGroup
	states := make([]contract.OperationState, 2)
	for i, subject := range []string{"cloud-admin-1", "cloud-admin-2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := executor.Execute(ctx, revokeClaim(t, subject))
			if err != nil {
				t.Error(err)
			}
			states[i] = result.State
		}()
	}
	wg.Wait()
	var enabledAdmins int
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_user WHERE site = ? AND enabled = 1 AND roles LIKE ?", memberTestSite, "%"+doctype.AdminRole+"%").Scan(&enabledAdmins); err != nil {
		t.Fatal(err)
	}
	if enabledAdmins != 1 {
		t.Fatalf("enabled administrators after concurrent revokes = %d (states %v), want exactly 1", enabledAdmins, states)
	}
}

func TestMySQLManagementStoreAcceptsRevokeKind(t *testing.T) {
	database, _, _ := newMySQLMemberFixture(t)
	store, err := sqlstore.New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	claim := revokeClaim(t, "cloud-user-1")
	input := management.AcceptCommand{ID: "01K7REVOKE00000000000000000000", Tenant: "tenant-123456789", Caller: "cloud-service", IdempotencyKey: "revoke-cloud-user-1", Kind: management.CommandTenantMemberRevoke, Command: claim.Operation.Command, Now: time.Now().UTC()}
	accepted, err := store.Accept(context.Background(), input)
	if err != nil || accepted.Kind != management.CommandTenantMemberRevoke {
		t.Fatalf("accept revoke = %+v %v", accepted, err)
	}
	replay, err := store.Accept(context.Background(), input)
	if err != nil || replay.ID != accepted.ID {
		t.Fatalf("idempotent replay = %+v %v", replay, err)
	}
}
