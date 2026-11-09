//go:build integration

package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/go-sql-driver/mysql"
)

func TestMySQLPurchaseApprovalStoreReadsLivePolicyInputs(t *testing.T) {
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:kora123@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable MySQL:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Skipf("MySQL unavailable: %v", err)
	}
	name := fmt.Sprintf("kora_purchase_policy_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create disposable database:", err)
	}
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`"); _ = root.Close() })
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse disposable DSN:", err)
	}
	config.DBName = name
	database, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		t.Fatal("open disposable tenant database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, statement := range []string{
		`CREATE TABLE _kora_auth_revision (id INT PRIMARY KEY, policy_revision BIGINT NOT NULL, membership_revision BIGINT NOT NULL)`,
		`INSERT INTO _kora_auth_revision VALUES (1, 7, 11)`,
		`CREATE TABLE _kora_user (site VARCHAR(160) NOT NULL, name VARCHAR(160) NOT NULL, enabled BOOLEAN NOT NULL, roles VARCHAR(512) NOT NULL, PRIMARY KEY(site, name))`,
		`INSERT INTO _kora_user VALUES ('tenant.example', 'requester', true, 'Procurement Officer'), ('tenant.example', 'finance', true, 'Finance Manager, Auditor'), ('tenant.example', 'disabled', false, 'Finance Manager'), ('other.example', 'wrong-site', true, 'Finance Manager')`,
		"CREATE TABLE `tabPurchase Request` (name VARCHAR(160) PRIMARY KEY, owner VARCHAR(160) NOT NULL, status VARCHAR(40) NOT NULL, estimated_total DECIMAL(12,2) NOT NULL, creation DATETIME NULL, modified DATETIME NULL, modified_by VARCHAR(160) NULL, doc_status INT NOT NULL, revision BIGINT NOT NULL)",
		"INSERT INTO `tabPurchase Request` (name, owner, status, estimated_total, doc_status, revision) VALUES ('PR-1', 'requester', 'Submitted', 500, 0, 3)",
	} {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare disposable approval policy schema: %v\n%s", err, statement)
		}
	}
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal("construct policy store:", err)
	}
	policyRevision, membershipRevision, err := store.AuthorizationRevisions(ctx)
	if err != nil || policyRevision != "7" || membershipRevision != "11" {
		t.Fatalf("AuthorizationRevisions() = (%q, %q, %v)", policyRevision, membershipRevision, err)
	}
	roles, active, err := store.UserRoles(ctx, "tenant.example", "finance")
	if err != nil || !active || len(roles) != 2 || roles[0] != "Finance Manager" || roles[1] != "Auditor" {
		t.Fatalf("UserRoles() = (%v, %v, %v)", roles, active, err)
	}
	users, err := store.UsersWithRoles(ctx, "tenant.example", []string{"Purchase Manager", "Finance Manager"}, 51)
	if err != nil || len(users) != 1 || users[0].Principal != "finance" {
		t.Fatalf("UsersWithRoles() = (%+v, %v)", users, err)
	}
	registry := doctype.NewRegistry()
	dt := &doctype.DocType{Name: "Purchase Request", Fields: []doctype.Field{{Fieldname: "status"}, {Fieldname: "estimated_total"}}}
	registry.Register(dt)
	document, owner, err := store.Subject(ctx, registry, contract.SubjectRef{Kind: dt.Name, ID: "PR-1", Revision: "3"})
	if err != nil || owner != "requester" || document.Revision != 3 || document.GetString("status") != "Submitted" || document.GetFloat("estimated_total") != 500 {
		t.Fatalf("Subject() = (doc=%+v owner=%q err=%v)", document, owner, err)
	}
	if _, err := store.UsersWithRoles(ctx, "tenant.example", []string{"Finance Manager"}, 0); err == nil {
		t.Fatal("UsersWithRoles accepted an unbounded/zero result limit")
	}
}
