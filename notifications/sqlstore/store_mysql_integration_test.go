//go:build integration

package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/notifications"
)

func TestMySQLNotificationReceiptAndClaimFence(t *testing.T) {
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:kora123@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open mysql:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Skipf("MySQL unavailable: %v", err)
	}
	name := fmt.Sprintf("kora_notification_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create isolated database:", err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`")
		_ = root.Close()
	})
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse mysql DSN:", err)
	}
	cfg.DBName = name
	database, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open isolated database:", err)
	}
	defer database.Close()
	dialect := db.Resolve("mysql")
	statements, err := SchemaStatements(dialect)
	if err != nil {
		t.Fatal("build schema:", err)
	}
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("apply schema: %v\n%s", err, statement)
		}
	}
	store, err := New(database, dialect)
	if err != nil {
		t.Fatal("create store:", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	tenant := contract.TenantID("tenant-a")
	eventID := "event-1"
	row := notifications.Delivery{ID: notifications.NotificationID(contract.NewID()), Tenant: tenant,
		Recipient: "user-1", RequestID: "request-1", Channel: notifications.ChannelEmail, EventID: eventID,
		Status: notifications.DeliveryPending, AvailableAt: now, CreatedAt: now}
	inserted, err := store.RecordEvent(context.Background(), tenant, eventID, []notifications.Delivery{row}, now)
	if err != nil || !inserted {
		t.Fatalf("RecordEvent = (%v, %v)", inserted, err)
	}
	inserted, err = store.RecordEvent(context.Background(), tenant, eventID, []notifications.Delivery{row}, now)
	if err != nil || inserted {
		t.Fatalf("duplicate RecordEvent = (%v, %v)", inserted, err)
	}
	exists, err := store.HasEvent(context.Background(), tenant, eventID)
	if err != nil || !exists {
		t.Fatalf("HasEvent = (%v, %v)", exists, err)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_notification_delivery`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("delivery count = %d, %v", count, err)
	}
	first, err := store.Claim(context.Background(), tenant, "worker-a", now, now.Add(time.Second), 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim = (%#v, %v)", first, err)
	}
	secondNow := now.Add(2 * time.Second)
	second, err := store.Claim(context.Background(), tenant, "worker-b", secondNow, secondNow.Add(time.Second), 10)
	if err != nil || len(second) != 1 || second[0].ClaimToken <= first[0].ClaimToken {
		t.Fatalf("reclaimed notification = (%#v, %v), first %#v", second, err, first)
	}
	if err := store.Accept(context.Background(), tenant, row.ID, "worker-a", first[0].ClaimToken, notifications.DeliveryResult{AcceptedAt: secondNow}, secondNow); err == nil {
		t.Fatal("stale claim was able to acknowledge delivery")
	}
	if err := store.Accept(context.Background(), tenant, row.ID, "worker-b", second[0].ClaimToken, notifications.DeliveryResult{AcceptedAt: secondNow}, secondNow); err != nil {
		t.Fatalf("current claim could not acknowledge delivery: %v", err)
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM _kora_notification_delivery WHERE id = ?`, string(row.ID)).Scan(&status); err != nil {
		t.Fatal("read acknowledged status:", err)
	}
	if status != notifications.DeliveryAccepted {
		t.Fatalf("delivery status = %q, want accepted", status)
	}
}
