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
	"github.com/asenawritescode/kora/internal/requests"
	"github.com/go-sql-driver/mysql"
)

// TestMySQLRequestInbox exercises real MySQL visibility, database-clock
// overdue filtering and keyset paging against a uniquely disposable schema.
func TestMySQLRequestInbox(t *testing.T) {
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:kora123@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open MySQL:", err)
	}
	if err := root.Ping(); err != nil {
		root.Close()
		t.Skipf("MySQL unavailable: %v", err)
	}
	name := fmt.Sprintf("kora_request_inbox_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		root.Close()
		t.Fatal("create isolated database:", err)
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
	defer database.Close()
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dialect := db.Resolve("mysql")
	statements, err := SchemaStatements(dialect)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply disposable request schema: %v", err)
		}
	}
	for _, statement := range db.OutboxTablesMySQL() {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply disposable outbox schema: %v", err)
		}
	}
	store, err := New(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for i, id := range []contract.RequestID{"01K7REQUEST00000000000000000", "01K7REQUEST00000000000000001"} {
		record := requests.RequestRecord{Tenant: "tenant-1", ID: id, Revision: "1", Kind: contract.RequestReview, AssignmentMode: contract.AssignmentAny,
			Status: contract.RequestPending, Requester: "alice", Title: fmt.Sprintf("Review %d", i), Message: "Please review", DueAt: timePtr(now.Add(-time.Minute)), CreatedAt: now, UpdatedAt: now}
		assignment := requests.AssignmentRecord{ID: contract.AssignmentID(fmt.Sprintf("01K7ASSIGNMENT0000000000000000%d", i)), RequestID: id, Principal: "bob",
			OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: "bob"}, Stage: 1, Status: contract.AssignmentPending,
			AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}, MembershipRevision: "1"}
		tx, err := database.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := insertRequest(ctx, tx, dialect, record); err != nil {
			_ = tx.Rollback()
			t.Fatal("insert disposable request:", err)
		}
		if err := insertAssignment(ctx, tx, dialect, record.Tenant, assignment, now); err != nil {
			_ = tx.Rollback()
			t.Fatal("insert disposable assignment:", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal("commit disposable request:", err)
		}
	}
	page, err := store.ListVisible(ctx, requests.VisibilityScope{Tenant: "tenant-1", Actor: "bob"}, contract.RequestFilter{Page: contract.PageRequest{Limit: 1}, View: contract.InboxAction})
	if err != nil {
		t.Fatal("list pending assigned actions:", err)
	}
	if len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("first action page = %#v", page)
	}
	second, err := store.ListVisible(ctx, requests.VisibilityScope{Tenant: "tenant-1", Actor: "bob"}, contract.RequestFilter{Page: contract.PageRequest{Limit: 1, Cursor: page.NextCursor}, View: contract.InboxAction})
	if err != nil {
		t.Fatal("list next action page:", err)
	}
	if len(second.Items) != 1 || second.Items[0].ID == page.Items[0].ID {
		t.Fatalf("second action page = %#v", second)
	}
	unrelated, err := store.ListVisible(ctx, requests.VisibilityScope{Tenant: "tenant-1", Actor: "mallory"}, contract.RequestFilter{Page: contract.PageRequest{Limit: 10}, View: contract.InboxCompleted})
	if err != nil {
		t.Fatal("list unrelated inbox:", err)
	}
	if len(unrelated.Items) != 0 {
		t.Fatalf("unrelated actor received requests: %#v", unrelated.Items)
	}
	overdue, err := store.ListVisible(ctx, requests.VisibilityScope{Tenant: "tenant-1", Actor: "alice"}, contract.RequestFilter{Page: contract.PageRequest{Limit: 10}, View: contract.InboxSent, OverdueOnly: true})
	if err != nil {
		t.Fatal("list overdue requests:", err)
	}
	if len(overdue.Items) != 2 {
		t.Fatalf("overdue sent count = %d, want 2", len(overdue.Items))
	}
	// Expiry uses the tenant database's clock, just like response and overdue
	// filtering. No worker timestamp can make replicas disagree at the deadline.
	expired, err := store.ExpireDue(ctx, "tenant-1", 10)
	if err != nil {
		t.Fatal("expire requests with skewed worker clock:", err)
	}
	if expired != 2 {
		t.Fatalf("database-clock expiry count = %d, want 2", expired)
	}
	var remaining int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_request WHERE tenant_id = ? AND status = ?`, "tenant-1", string(contract.RequestPending)).Scan(&remaining); err != nil {
		t.Fatal("count pending requests after expiry:", err)
	}
	if remaining != 0 {
		t.Fatalf("pending requests after database-clock expiry = %d, want 0", remaining)
	}
	var events int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND event_type = ?`, "tenant-1", "request.expired").Scan(&events); err != nil {
		t.Fatal("count expiry events:", err)
	}
	if events != 2 {
		t.Fatalf("expiry events = %d, want 2", events)
	}
}

func timePtr(value time.Time) *time.Time { return &value }
