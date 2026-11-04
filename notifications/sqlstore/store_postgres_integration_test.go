//go:build integration

package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/notifications"
	_ "github.com/lib/pq"
)

func TestPostgresNotificationReceiptAndClaimFence(t *testing.T) {
	dsn := os.Getenv("KORA_NOTIFICATIONS_DISPOSABLE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_NOTIFICATIONS_DISPOSABLE_POSTGRES_DSN to a disposable PostgreSQL service")
	}
	root, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Fatal("ping disposable PostgreSQL:", err)
	}
	schemaName := fmt.Sprintf("kora_notification_%x", time.Now().UnixNano())
	if _, err := root.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		_ = root.Close()
		t.Fatal("create isolated notification schema:", err)
	}
	t.Cleanup(func() { _, _ = root.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`); _ = root.Close() })
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("parse disposable PostgreSQL DSN:", err)
	}
	query := parsed.Query()
	query.Set("search_path", schemaName)
	parsed.RawQuery = query.Encode()
	database, err := sql.Open("postgres", parsed.String())
	if err != nil {
		t.Fatal("open isolated notification schema:", err)
	}
	defer database.Close()
	dialect := db.Resolve("postgres")
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
	tenant := contract.TenantID("tenant-" + contract.NewID())
	eventID := "event-" + contract.NewID()
	row := notifications.Delivery{ID: notifications.NotificationID(contract.NewID()), Tenant: tenant,
		Recipient: "user-1", RequestID: contract.RequestID(contract.NewID()), Channel: notifications.ChannelInbox, EventID: eventID,
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
	first, err := store.Claim(context.Background(), tenant, "worker-a", now, now.Add(time.Second), 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim = (%#v, %v)", first, err)
	}
	secondNow := now.Add(2 * time.Second)
	second, err := store.Claim(context.Background(), tenant, "worker-b", secondNow, secondNow.Add(time.Second), 10)
	if err != nil || len(second) != 1 || second[0].ClaimToken <= first[0].ClaimToken {
		t.Fatalf("reclaimed delivery = (%#v, %v)", second, err)
	}
	if err := store.Accept(context.Background(), tenant, row.ID, "worker-a", first[0].ClaimToken, notifications.DeliveryResult{AcceptedAt: secondNow}, secondNow); err == nil {
		t.Fatal("stale claim acknowledged delivery")
	}
	if err := store.Accept(context.Background(), tenant, row.ID, "worker-b", second[0].ClaimToken, notifications.DeliveryResult{AcceptedAt: secondNow}, secondNow); err != nil {
		t.Fatalf("current claim could not acknowledge delivery: %v", err)
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM _kora_notification_delivery WHERE id = $1`, string(row.ID)).Scan(&status); err != nil || status != notifications.DeliveryAccepted {
		t.Fatalf("delivery status=%q err=%v", status, err)
	}
}
