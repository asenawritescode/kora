//go:build integration

package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/analytics"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/eventstream"
	"github.com/asenawritescode/kora/internal/notifications"
	notificationsql "github.com/asenawritescode/kora/internal/notifications/sqlstore"
	"github.com/asenawritescode/kora/internal/outbox"
	requestsql "github.com/asenawritescode/kora/internal/requests/sqlstore"
	"github.com/go-sql-driver/mysql"
)

func TestMySQLRequestOutboxSchedulesAndDeliversRecipientScopedInboxEvent(t *testing.T) {
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
	name := fmt.Sprintf("kora_engine_notification_%d", time.Now().UnixNano()%1_000_000_000)
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
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dialect := db.Resolve("mysql")
	for _, statement := range db.OutboxTablesMySQL() {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply outbox schema: %v\n%s", err, statement)
		}
	}
	requestDDL, err := requestsql.SchemaStatements(dialect)
	if err != nil {
		t.Fatal("build request schema:", err)
	}
	for _, statement := range requestDDL {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply request schema: %v\n%s", err, statement)
		}
	}
	notificationDDL, err := notificationsql.SchemaStatements(dialect)
	if err != nil {
		t.Fatal("build notification schema:", err)
	}
	for _, statement := range notificationDDL {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply notification schema: %v\n%s", err, statement)
		}
	}
	if _, err := database.ExecContext(ctx, `CREATE TABLE _kora_user (site VARCHAR(140) NOT NULL, name VARCHAR(160) NOT NULL, enabled BOOLEAN NOT NULL, PRIMARY KEY(site,name))`); err != nil {
		t.Fatal("create user table:", err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO _kora_user (site,name,enabled) VALUES ('tenant-route','alice',TRUE),('tenant-route','bob',TRUE)`); err != nil {
		t.Fatal("insert active users:", err)
	}
	const tenant contract.TenantID = "stable-tenant-notification-e2e"
	const requestID contract.RequestID = "01K7NOTIFYE2EREQUEST0000000000"
	const assignmentID contract.AssignmentID = "01K7NOTIFYE2EASSIGNMENT00000000"
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := database.ExecContext(ctx, `INSERT INTO _kora_request
		(id,tenant_id,revision,kind,assignment_mode,status,requester_id,title,message,created_at,updated_at)
		VALUES (?,?,1,'task','any','pending','alice','Review task','Please review',?,?)`, string(requestID), string(tenant), now, now); err != nil {
		t.Fatal("insert direct task fixture:", err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO _kora_request_assignment
		(id,tenant_id,request_id,stage,assignee_kind,assignee_id,original_assignee_kind,original_assignee_id,status,allowed_responses,membership_revision,created_at,updated_at)
		VALUES (?,?,?,1,'user','bob','user','bob','pending',JSON_ARRAY('complete'),'1',?,?)`, string(assignmentID), string(tenant), string(requestID), now, now); err != nil {
		t.Fatal("insert assignment fixture:", err)
	}
	eventData, _ := json.Marshal(map[string]any{"request_id": string(requestID), "revision": "1", "status": "pending"})
	event := contract.EventEnvelope{ID: contract.NewEventID(), Type: "request.created", Version: contract.CurrentVersion,
		Source: "kora.engine", Site: "tenant-route", AggregateType: "request", AggregateID: string(requestID), OccurredAt: now, Data: eventData}
	writer := outbox.NewSQLWriter(dialect)
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin request event transaction:", err)
	}
	if err := writer.Append(ctx, tx, event); err != nil {
		_ = tx.Rollback()
		t.Fatal("append committed request event:", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal("commit request event:", err)
	}
	requestStore, err := requestsql.New(database, dialect)
	if err != nil {
		t.Fatal("construct request store:", err)
	}
	notificationStore, err := notificationsql.New(database, dialect)
	if err != nil {
		t.Fatal("construct notification store:", err)
	}
	service, err := notifications.NewService(tenant, notificationStore, requestNotificationPlanner{store: requestStore, tenant: tenant, site: "tenant-route"}, requestWallClock{}, requestIDs{})
	if err != nil {
		t.Fatal("construct notification service:", err)
	}
	dispatcher := outbox.NewDispatcher(database, dialect)
	if err := dispatcher.RegisterForTypes("request-notifications", []string{"request.created"}, service.Consume); err != nil {
		t.Fatal("register outbox notification consumer:", err)
	}
	processed, err := dispatcher.Run(ctx, 10)
	if err != nil || processed != 1 {
		t.Fatalf("dispatch request event = (%d, %v), want (1, nil)", processed, err)
	}
	var scheduled int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_notification_delivery WHERE tenant_id=? AND request_id=? AND recipient_id='bob' AND channel='inbox'`, string(tenant), string(requestID)).Scan(&scheduled); err != nil {
		t.Fatal("count scheduled inbox delivery:", err)
	}
	if scheduled != 1 {
		t.Fatalf("scheduled inbox deliveries=%d, want 1", scheduled)
	}
	failingWorker, err := notifications.NewWorker(tenant, notificationStore, requestNotificationFailureSender{}, requestWallClock{}, "notification-failure-worker")
	if err != nil {
		t.Fatal("construct failing notification worker:", err)
	}
	accepted, err := failingWorker.RunOnce(ctx)
	if err != nil || accepted != 0 {
		t.Fatalf("failed notification attempt = (%d, %v), want (0, nil) with durable retry", accepted, err)
	}
	var deliveryStatus, lastError string
	var attempts int
	if err := database.QueryRowContext(ctx, `SELECT status, attempts, last_error FROM _kora_notification_delivery WHERE tenant_id=? AND request_id=?`, string(tenant), string(requestID)).Scan(&deliveryStatus, &attempts, &lastError); err != nil {
		t.Fatal("read failed notification attempt:", err)
	}
	if deliveryStatus != notifications.DeliveryPending || attempts != 1 || lastError == "" {
		t.Fatalf("failed notification persistence status=%q attempts=%d last_error=%q", deliveryStatus, attempts, lastError)
	}
	var requestStatus string
	if err := database.QueryRowContext(ctx, `SELECT status FROM _kora_request WHERE tenant_id=? AND id=?`, string(tenant), string(requestID)).Scan(&requestStatus); err != nil {
		t.Fatal("read request after notification failure:", err)
	}
	if requestStatus != string(contract.RequestPending) {
		t.Fatalf("notification failure changed durable request status to %q", requestStatus)
	}
	if _, err := database.ExecContext(ctx, `UPDATE _kora_notification_delivery SET available_at=? WHERE tenant_id=? AND request_id=?`, now, string(tenant), string(requestID)); err != nil {
		t.Fatal("make persisted notification retry due:", err)
	}
	bus := analytics.NewChannelBusWithLimits(2, 2048, 4096, t.TempDir())
	defer bus.Close()
	events, err := bus.Subscribe()
	if err != nil {
		t.Fatal("subscribe to tenant event bus:", err)
	}
	worker, err := notifications.NewWorker(tenant, notificationStore, inboxNotificationSender{site: "tenant-route", bus: bus}, requestWallClock{}, "notification-test-worker")
	if err != nil {
		t.Fatal("construct notification worker:", err)
	}
	accepted, err = worker.RunOnce(ctx)
	if err != nil || accepted != 1 {
		t.Fatalf("deliver inbox notification = (%d, %v), want (1, nil)", accepted, err)
	}
	select {
	case pushed := <-events:
		if pushed.Operation != eventstream.EventNotification || pushed.Data["recipient"] != "bob" || pushed.Data["request_id"] != string(requestID) {
			t.Fatalf("published notification event = %#v", pushed)
		}
	case <-time.After(time.Second):
		t.Fatal("notification worker did not publish recipient invalidation")
	}
	if err := database.QueryRowContext(ctx, `SELECT status FROM _kora_notification_delivery WHERE tenant_id=? AND request_id=?`, string(tenant), string(requestID)).Scan(&deliveryStatus); err != nil {
		t.Fatal("read persisted notification status:", err)
	}
	if deliveryStatus != notifications.DeliveryAccepted {
		t.Fatalf("notification status=%q, want accepted", deliveryStatus)
	}
	processed, err = dispatcher.Run(ctx, 10)
	if err != nil || processed != 0 {
		t.Fatalf("duplicate outbox notification delivery = (%d, %v), want no repeated plan", processed, err)
	}
}

type requestNotificationFailureSender struct{}

func (requestNotificationFailureSender) Send(context.Context, notifications.Delivery) (notifications.DeliveryResult, error) {
	return notifications.DeliveryResult{}, errors.New("temporary provider failure")
}
