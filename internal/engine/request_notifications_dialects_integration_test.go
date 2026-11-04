//go:build integration

package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/eventstream"
	"github.com/asenawritescode/kora/notifications"
	notificationsql "github.com/asenawritescode/kora/notifications/sqlstore"
	"github.com/asenawritescode/kora/outbox"
	requestsql "github.com/asenawritescode/kora/requests/sqlstore"
	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestPostgresRequestOutboxSchedulesAndRecoversInboxDelivery(t *testing.T) {
	dsn := os.Getenv("KORA_REQUESTS_DISPOSABLE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_REQUESTS_DISPOSABLE_POSTGRES_DSN to a disposable PostgreSQL service")
	}
	root, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Fatal("ping disposable PostgreSQL:", err)
	}
	schemaName := fmt.Sprintf("kora_notify_engine_%x", time.Now().UnixNano())
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
	runRequestNotificationOutboxLifecycle(t, database, db.Resolve("postgres"), db.OutboxTablesPostgres())
}

func TestLibSQLRemoteRequestOutboxSchedulesAndRecoversInboxDelivery(t *testing.T) {
	dsn := os.Getenv("KORA_REQUESTS_DISPOSABLE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_REQUESTS_DISPOSABLE_LIBSQL_DSN to a fresh disposable LibSQL HTTP database")
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable LibSQL HTTP endpoint:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(4)
	runRequestNotificationOutboxLifecycle(t, database, db.Resolve("libsql"), db.OutboxTablesLibSQL())
}

func runRequestNotificationOutboxLifecycle(t *testing.T, database *sql.DB, dialect db.Dialect, outboxDDL []string) {
	t.Helper()
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, statement := range outboxDDL {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply %s outbox schema: %v\n%s", dialect.DriverName(), err, statement)
		}
	}
	requestDDL, err := requestsql.SchemaStatements(dialect)
	if err != nil {
		t.Fatal("build request schema:", err)
	}
	for _, statement := range requestDDL {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply %s request schema: %v\n%s", dialect.DriverName(), err, statement)
		}
	}
	notificationDDL, err := notificationsql.SchemaStatements(dialect)
	if err != nil {
		t.Fatal("build notification schema:", err)
	}
	for _, statement := range notificationDDL {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply %s notification schema: %v\n%s", dialect.DriverName(), err, statement)
		}
	}
	if _, err := database.ExecContext(ctx, `CREATE TABLE _kora_user (site TEXT NOT NULL, name TEXT NOT NULL, enabled SMALLINT NOT NULL, PRIMARY KEY(site,name))`); err != nil {
		t.Fatalf("%s create user table: %v", dialect.DriverName(), err)
	}
	if _, err := database.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO _kora_user (site,name,enabled) VALUES (?,?,?),(?,?,?)`), "tenant-route", "alice", 1, "tenant-route", "bob", 1); err != nil {
		t.Fatalf("%s insert active users: %v", dialect.DriverName(), err)
	}
	tenant := contract.TenantID("tenant-" + contract.NewID())
	requestID := contract.RequestID(contract.NewID())
	assignmentID := contract.AssignmentID(contract.NewID())
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := database.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO _kora_request
		(id,tenant_id,revision,kind,assignment_mode,status,requester_id,title,message,created_at,updated_at)
		VALUES (?, ?, 1, 'task', 'any', 'pending', 'alice', 'Review task', 'Please review', ?, ?)`), string(requestID), string(tenant), now, now); err != nil {
		t.Fatalf("%s insert task fixture: %v", dialect.DriverName(), err)
	}
	if _, err := database.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO _kora_request_assignment
		(id,tenant_id,request_id,stage,assignee_kind,assignee_id,original_assignee_kind,original_assignee_id,status,allowed_responses,membership_revision,created_at,updated_at)
		VALUES (?, ?, ?, 1, 'user', 'bob', 'user', 'bob', 'pending', ?, '1', ?, ?)`), string(assignmentID), string(tenant), string(requestID), `["complete"]`, now, now); err != nil {
		t.Fatalf("%s insert assignment fixture: %v", dialect.DriverName(), err)
	}
	eventData, _ := json.Marshal(map[string]any{"request_id": string(requestID), "revision": "1", "status": "pending"})
	event := contract.EventEnvelope{ID: contract.NewEventID(), Type: "request.created", Version: contract.CurrentVersion,
		Source: "kora.engine", Site: "tenant-route", AggregateType: "request", AggregateID: string(requestID), OccurredAt: now, Data: eventData}
	writer := outbox.NewSQLWriter(dialect)
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("%s begin request event transaction: %v", dialect.DriverName(), err)
	}
	if err := writer.Append(ctx, tx, event); err != nil {
		_ = tx.Rollback()
		t.Fatalf("%s append request event: %v", dialect.DriverName(), err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("%s commit request event: %v", dialect.DriverName(), err)
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
		t.Fatal("register request notification consumer:", err)
	}
	processed, err := dispatcher.Run(ctx, 10)
	if err != nil || processed != 1 {
		t.Fatalf("%s dispatch request event = (%d, %v)", dialect.DriverName(), processed, err)
	}
	var scheduled int
	if err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT COUNT(*) FROM _kora_notification_delivery WHERE tenant_id=? AND request_id=? AND recipient_id='bob' AND channel='inbox'`), string(tenant), string(requestID)).Scan(&scheduled); err != nil || scheduled != 1 {
		t.Fatalf("%s scheduled inbox deliveries=%d err=%v", dialect.DriverName(), scheduled, err)
	}
	failingWorker, err := notifications.NewWorker(tenant, notificationStore, requestNotificationFailureSender{}, requestWallClock{}, "notification-failure-worker")
	if err != nil {
		t.Fatal("construct failing notification worker:", err)
	}
	accepted, err := failingWorker.RunOnce(ctx)
	if err != nil || accepted != 0 {
		t.Fatalf("%s failed notification attempt=(%d, %v)", dialect.DriverName(), accepted, err)
	}
	var deliveryStatus, lastError string
	var attempts int
	if err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT status, attempts, last_error FROM _kora_notification_delivery WHERE tenant_id=? AND request_id=?`), string(tenant), string(requestID)).Scan(&deliveryStatus, &attempts, &lastError); err != nil {
		t.Fatalf("%s read failed notification attempt: %v", dialect.DriverName(), err)
	}
	if deliveryStatus != notifications.DeliveryPending || attempts != 1 || lastError == "" {
		t.Fatalf("%s failed delivery persistence status=%q attempts=%d error=%q", dialect.DriverName(), deliveryStatus, attempts, lastError)
	}
	var requestStatus string
	if err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT status FROM _kora_request WHERE tenant_id=? AND id=?`), string(tenant), string(requestID)).Scan(&requestStatus); err != nil || requestStatus != string(contract.RequestPending) {
		t.Fatalf("%s notification failure changed request state=%q err=%v", dialect.DriverName(), requestStatus, err)
	}
	if _, err := database.ExecContext(ctx, db.Rebind(dialect, `UPDATE _kora_notification_delivery SET available_at=? WHERE tenant_id=? AND request_id=?`), now, string(tenant), string(requestID)); err != nil {
		t.Fatalf("%s make retry due: %v", dialect.DriverName(), err)
	}
	bus := analytics.NewChannelBusWithLimits(2, 2048, 4096, t.TempDir())
	defer bus.Close()
	events, err := bus.Subscribe()
	if err != nil {
		t.Fatal("subscribe tenant event bus:", err)
	}
	worker, err := notifications.NewWorker(tenant, notificationStore, inboxNotificationSender{site: "tenant-route", bus: bus}, requestWallClock{}, "notification-recovery-worker")
	if err != nil {
		t.Fatal("construct notification retry worker:", err)
	}
	accepted, err = worker.RunOnce(ctx)
	if err != nil || accepted != 1 {
		t.Fatalf("%s recovered notification=(%d, %v)", dialect.DriverName(), accepted, err)
	}
	select {
	case pushed := <-events:
		if pushed.Operation != eventstream.EventNotification || pushed.Data["recipient"] != "bob" || pushed.Data["request_id"] != string(requestID) {
			t.Fatalf("%s published event=%#v", dialect.DriverName(), pushed)
		}
	case <-time.After(time.Second):
		t.Fatalf("%s worker did not publish recipient invalidation", dialect.DriverName())
	}
	if err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT status FROM _kora_notification_delivery WHERE tenant_id=? AND request_id=?`), string(tenant), string(requestID)).Scan(&deliveryStatus); err != nil || deliveryStatus != notifications.DeliveryAccepted {
		t.Fatalf("%s delivery status=%q err=%v", dialect.DriverName(), deliveryStatus, err)
	}
	processed, err = dispatcher.Run(ctx, 10)
	if err != nil || processed != 0 {
		t.Fatalf("%s duplicate outbox delivery=(%d, %v)", dialect.DriverName(), processed, err)
	}
}
