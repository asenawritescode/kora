//go:build integration

package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/go-sql-driver/mysql"
)

func TestMySQLDirectTaskNotificationRecipientsAreCurrentAndScoped(t *testing.T) {
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
	name := fmt.Sprintf("kora_request_notification_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create disposable database:", err)
	}
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`"); _ = root.Close() })
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse disposable DSN:", err)
	}
	cfg.DBName = name
	database, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open disposable tenant database:", err)
	}
	defer database.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dialect := db.Resolve("mysql")
	statements, err := SchemaStatements(dialect)
	if err != nil {
		t.Fatal("build request schema:", err)
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply request schema: %v\n%s", err, statement)
		}
	}
	if _, err := database.ExecContext(ctx, `CREATE TABLE _kora_user (site VARCHAR(140) NOT NULL, name VARCHAR(160) NOT NULL, enabled BOOLEAN NOT NULL, PRIMARY KEY(site,name))`); err != nil {
		t.Fatal("create disposable user table:", err)
	}
	for _, user := range []struct {
		name    string
		enabled bool
	}{{"alice", true}, {"bob", true}, {"carol", false}, {"mallory", true}} {
		if _, err := database.ExecContext(ctx, `INSERT INTO _kora_user (site,name,enabled) VALUES (?,?,?)`, "tenant-route", user.name, user.enabled); err != nil {
			t.Fatalf("insert disposable user %q: %v", user.name, err)
		}
	}
	store, err := New(database, dialect)
	if err != nil {
		t.Fatal("construct request store:", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	const tenant contract.TenantID = "stable-tenant-notification"
	const directID contract.RequestID = "01K7NOTIFYDIRECT0000000000000"
	const approvalID contract.RequestID = "01K7NOTIFYAPPROVAL00000000000"
	insertRequest := func(id contract.RequestID, kind, requester, policyID, subjectKind string) {
		t.Helper()
		_, err := database.ExecContext(ctx, `INSERT INTO _kora_request
			(id,tenant_id,revision,kind,assignment_mode,status,requester_id,subject_kind,subject_digest,title,message,policy_id,created_at,updated_at)
			VALUES (?,?,1,?,'sequential','pending',?,?,?,?,?,?,?,?)`, string(id), string(tenant), kind, requester,
			nullOrString(subjectKind), "", "Notify", "Please review", policyID, now, now)
		if err != nil {
			t.Fatalf("insert request %s: %v", id, err)
		}
	}
	insertRequest(directID, "task", "alice", "", "")
	insertRequest(approvalID, "approval", "alice", "invoice-approval-v1", "Invoice")
	insertAssignment := func(id, requestID, assignee, status string) {
		t.Helper()
		if _, err := database.ExecContext(ctx, `INSERT INTO _kora_request_assignment
			(id,tenant_id,request_id,stage,assignee_kind,assignee_id,original_assignee_kind,original_assignee_id,status,allowed_responses,membership_revision,created_at,updated_at)
			VALUES (?,?,?,1,'user',?,'user',?,?,JSON_ARRAY('complete'),'1',?,?)`, id, string(tenant), requestID, assignee, assignee, status, now, now); err != nil {
			t.Fatalf("insert assignment %s: %v", id, err)
		}
	}
	insertAssignment("01K7NOTIFYASSIGNBOB00000000000", string(directID), "bob", "pending")
	insertAssignment("01K7NOTIFYASSIGNCAROL000000000", string(directID), "carol", "blocked")
	insertAssignment("01K7NOTIFYASSIGNMALLORY0000000", string(approvalID), "mallory", "pending")

	created, err := store.DirectTaskNotificationRecipients(ctx, tenant, "tenant-route", directID, "request.created")
	if err != nil {
		t.Fatal("plan created-task recipients:", err)
	}
	if !reflect.DeepEqual(created, []contract.PrincipalID{"bob"}) {
		t.Fatalf("created-task recipients = %#v, want only current active assignee bob", created)
	}
	if _, err := database.ExecContext(ctx, `UPDATE _kora_request_assignment SET status='responded' WHERE tenant_id=? AND request_id=? AND assignee_id='bob'`, string(tenant), string(directID)); err != nil {
		t.Fatal("mark direct task assignment responded:", err)
	}
	responded, err := store.DirectTaskNotificationRecipients(ctx, tenant, "tenant-route", directID, "request.responded")
	if err != nil {
		t.Fatal("plan response recipients:", err)
	}
	if !reflect.DeepEqual(responded, []contract.PrincipalID{"alice"}) {
		t.Fatalf("response recipients = %#v, want active requester only after no pending assignee remains", responded)
	}
	approvalRecipients, err := store.DirectTaskNotificationRecipients(ctx, tenant, "tenant-route", approvalID, "request.created")
	if err != nil {
		t.Fatal("resolve governed approval recipients:", err)
	}
	if len(approvalRecipients) != 0 {
		t.Fatalf("direct-task planner scheduled governed approval recipients: %#v", approvalRecipients)
	}
	if _, err := store.DirectTaskNotificationRecipients(ctx, tenant, "tenant-route", directID, "request.unknown"); err == nil {
		t.Fatal("unknown request event type was accepted")
	}
}

func nullOrString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
