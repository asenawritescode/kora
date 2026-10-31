//go:build integration

package sqlstore

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/tursodatabase/libsql-client-go/libsql"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/notifications"
)

func TestLibSQLNotificationReceiptAndClaimFence(t *testing.T) {
	dsn := os.Getenv("KORA_REQUESTS_DISPOSABLE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_REQUESTS_DISPOSABLE_LIBSQL_DSN to an isolated disposable LibSQL database")
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open LibSQL:", err)
	}
	defer database.Close()
	dialect := db.Resolve("libsql")
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
	now := time.Now().UTC().Truncate(time.Millisecond)
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
	first, err := store.Claim(context.Background(), tenant, "worker-a", now, now.Add(time.Second), 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim = (%#v, %v)", first, err)
	}
	secondNow := now.Add(2 * time.Second)
	second, err := store.Claim(context.Background(), tenant, "worker-b", secondNow, secondNow.Add(time.Second), 10)
	if err != nil || len(second) != 1 || second[0].ClaimToken <= first[0].ClaimToken {
		t.Fatalf("reclaimed notification = (%#v, %v)", second, err)
	}
	if err := store.Accept(context.Background(), tenant, row.ID, "worker-a", first[0].ClaimToken, notifications.DeliveryResult{AcceptedAt: secondNow}, secondNow); err == nil {
		t.Fatal("stale claim acknowledged delivery")
	}
	if err := store.Accept(context.Background(), tenant, row.ID, "worker-b", second[0].ClaimToken, notifications.DeliveryResult{AcceptedAt: secondNow}, secondNow); err != nil {
		t.Fatalf("current claim could not acknowledge delivery: %v", err)
	}
}
