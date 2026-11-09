//go:build integration

package engine

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/disposabledb"
	notificationsql "github.com/asenawritescode/kora/internal/notifications/sqlstore"
	requestsql "github.com/asenawritescode/kora/internal/requests/sqlstore"
	schedulersql "github.com/asenawritescode/kora/internal/scheduler/sqlstore"
	"github.com/asenawritescode/kora/internal/script"
	workflowsql "github.com/asenawritescode/kora/internal/workflow/sqlstore"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestDurableWorkScannerFindsUnreceiptedTenantWorkAcrossDialects(t *testing.T) {
	cases := []struct {
		name   string
		driver string
		key    string
	}{
		{name: "mysql", driver: "mysql", key: "KORA_DURABLE_SCAN_DISPOSABLE_MYSQL_DSN"},
		{name: "postgres", driver: "postgres", key: "KORA_DURABLE_SCAN_DISPOSABLE_POSTGRES_DSN"},
		{name: "libsql-http", driver: "libsql", key: "KORA_DURABLE_SCAN_DISPOSABLE_LIBSQL_DSN"},
	}
	run := false
	for _, tc := range cases {
		if os.Getenv(tc.key) != "" {
			run = true
		}
	}
	if !run {
		t.Skip("set KORA_DURABLE_SCAN_DISPOSABLE_{MYSQL,POSTGRES,LIBSQL}_DSN to fresh disposable databases")
	}
	for _, tc := range cases {
		dsn := os.Getenv(tc.key)
		if dsn == "" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			for _, key := range []string{"DB_DSN", "KORA_DB_TYPE", "KORA_DB_HOST", "KORA_DB_USER", "KORA_DB_PASSWORD", "KORA_DB_PORT"} {
				if value, ok := os.LookupEnv(key); ok {
					_ = os.Unsetenv(key)
					t.Cleanup(func() { _ = os.Setenv(key, value) })
				}
			}
			if tc.driver != "libsql" {
				// The probe asserts an empty database: never share one.
				dsn = disposabledb.Open(t, tc.driver, dsn).DSN
			}
			database, err := dbaccess.Open(tc.driver, dsn)
			if err != nil {
				t.Fatal("open disposable tenant database:", err)
			}
			defer database.Close()
			if err := database.PingContext(t.Context()); err != nil {
				t.Fatal("ping disposable tenant database:", err)
			}
			dialect := db.Resolve(tc.driver)
			if due, err := hasDueTenantWork(t.Context(), database, dialect); err != nil || due {
				t.Fatalf("initial disposable durable-work probe = (%t, %v), want (false, nil)", due, err)
			}
			lifecycleStore := &script.Store{DB: database, Dialect: dialect}
			if err := lifecycleStore.EnsureAsyncHookInvocationSchema(t.Context()); err != nil {
				t.Fatal("ensure durable lifecycle hook invocation schema:", err)
			}
			invocationID := strings.Repeat("d", 64)
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `INSERT INTO _kora_script_invocation (id,tenant_id,source_operation_id,sequence_no,status,payload_json,next_attempt_at,last_error,created_at,updated_at) VALUES (?,?,?,1,'pending',?,CURRENT_TIMESTAMP,'',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`), invocationID, "scanner-test", contract.NewOperationID(), `{}`); err != nil {
				t.Fatal("insert due lifecycle hook invocation:", err)
			}
			if due, err := hasDueTenantWork(t.Context(), database, dialect); err != nil || !due {
				t.Fatalf("durable-work probe for pending lifecycle hook = (%t, %v), want (true, nil)", due, err)
			}
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `UPDATE _kora_script_invocation SET status='running',lease_until=CURRENT_TIMESTAMP,external_effect_started=TRUE WHERE id=?`), invocationID); err != nil {
				t.Fatal("expire external lifecycle invocation lease:", err)
			}
			if due, err := hasDueTenantWork(t.Context(), database, dialect); err != nil || !due {
				t.Fatalf("durable-work probe for uncertain lifecycle hook = (%t, %v), want (true, nil)", due, err)
			}
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `DELETE FROM _kora_script_invocation WHERE id=?`), invocationID); err != nil {
				t.Fatal("remove lifecycle hook fixture:", err)
			}
			cursorStore, err := newDurableScanStateStore(database, dialect)
			if err != nil {
				t.Fatal("construct durable scan cursor store:", err)
			}
			if err := cursorStore.Ensure(t.Context()); err != nil {
				t.Fatal("ensure durable scan cursor table:", err)
			}
			cursorKey := "integration:" + contract.NewID()
			cursor, completed, err := cursorStore.Load(t.Context(), cursorKey)
			if err != nil || cursor != "" || !completed.IsZero() {
				t.Fatalf("initial cursor = (%q, %v), want empty", cursor, err)
			}
			if err := cursorStore.Advance(t.Context(), cursorKey, "", "tenant-a", false); err != nil {
				t.Fatal("advance durable scan cursor:", err)
			}
			if err := cursorStore.Advance(t.Context(), cursorKey, "", "tenant-b", false); err != ErrDurableScanCursorMoved {
				t.Fatalf("stale cursor advance = %v, want cursor moved", err)
			}
			cursor, completed, err = cursorStore.Load(t.Context(), cursorKey)
			if err != nil || cursor != "tenant-a" {
				t.Fatalf("cursor after CAS = (%q, %v), want tenant-a", cursor, err)
			}
			if err := cursorStore.Advance(t.Context(), cursorKey, "tenant-a", "", true); err != nil {
				t.Fatal("complete durable scan sweep:", err)
			}
			_, completed, err = cursorStore.Load(t.Context(), cursorKey)
			if err != nil || completed.IsZero() {
				t.Fatalf("persisted completed sweep time = (%v, %v), want nonzero", completed, err)
			}
			statements := outboxSchema(tc.driver)
			for _, statement := range statements {
				if _, err := database.ExecContext(t.Context(), statement); err != nil {
					t.Fatalf("create outbox fixture: %v\n%s", err, statement)
				}
			}
			eventID := contract.NewID()
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `INSERT INTO _kora_outbox (id,site,event_type,status) VALUES (?,?,?,'published')`), eventID, "scanner-test", "kora.invoice.after_save"); err != nil {
				t.Fatal("insert unreceipted durable webhook event:", err)
			}
			due, err := hasDueTenantWork(t.Context(), database, dialect)
			if err != nil || !due {
				t.Fatalf("hasDueTenantWork without webhook receipt = (%t, %v), want (true, nil)", due, err)
			}
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `INSERT INTO _kora_outbox_receipt (consumer_name,event_id,site) VALUES (?,?,?)`), "engine-webhooks-v1", eventID, "scanner-test"); err != nil {
				t.Fatal("insert webhook receipt:", err)
			}
			due, err = hasDueTenantWork(t.Context(), database, dialect)
			if err != nil || due {
				t.Fatalf("hasDueTenantWork after webhook receipt = (%t, %v), want (false, nil)", due, err)
			}
			for _, statement := range analyticsScannerSchema(tc.driver) {
				if _, err := database.ExecContext(t.Context(), statement); err != nil {
					t.Fatalf("create analytics rebuild fixture: %v\n%s", err, statement)
				}
			}
			scheduleStore, err := schedulersql.New(database, dialect)
			if err != nil {
				t.Fatal("construct scheduler fixture store:", err)
			}
			if err := scheduleStore.Ensure(t.Context()); err != nil {
				t.Fatal("create scheduler fixture schema:", err)
			}
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `INSERT INTO _kora_schedule_definition (tenant_id,job_name,definition_digest,schedule,job_json,next_run_at,updated_at) VALUES (?,?,?,'* * * * *','{}',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`), "scanner-test", "due-job", strings.Repeat("a", 64)); err != nil {
				t.Fatal("insert due scheduled job:", err)
			}
			due, err = hasDueTenantWork(t.Context(), database, dialect)
			if err != nil || !due {
				t.Fatalf("hasDueTenantWork for persisted due schedule = (%t, %v), want (true, nil)", due, err)
			}
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `DELETE FROM _kora_schedule_definition WHERE job_name=?`), "due-job"); err != nil {
				t.Fatal("remove due scheduled job fixture:", err)
			}
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `INSERT INTO _kora_schedule_occurrence (tenant_id,job_name,scheduled_at,definition_digest,job_json,state,last_error) VALUES (?,?,CURRENT_TIMESTAMP,?,'{}','pending','')`), "scanner-test", "persisted-occurrence", strings.Repeat("b", 64)); err != nil {
				t.Fatal("insert pending scheduled occurrence:", err)
			}
			due, err = hasDueTenantWork(t.Context(), database, dialect)
			if err != nil || !due {
				t.Fatalf("hasDueTenantWork for persisted scheduled occurrence = (%t, %v), want (true, nil)", due, err)
			}
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `DELETE FROM _kora_schedule_occurrence WHERE job_name=?`), "persisted-occurrence"); err != nil {
				t.Fatal("remove scheduled occurrence fixture:", err)
			}
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `INSERT INTO _kora_schedule_occurrence (tenant_id,job_name,scheduled_at,definition_digest,job_json,state,lease_until,last_error) VALUES (?,?,CURRENT_TIMESTAMP,?,'{}','running',CURRENT_TIMESTAMP,'')`), "scanner-test", "expired-occurrence", strings.Repeat("c", 64)); err != nil {
				t.Fatal("insert expired scheduled occurrence:", err)
			}
			due, err = hasDueTenantWork(t.Context(), database, dialect)
			if err != nil || !due {
				t.Fatalf("hasDueTenantWork for expired scheduled occurrence = (%t, %v), want (true, nil)", due, err)
			}
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `DELETE FROM _kora_schedule_occurrence WHERE job_name=?`), "expired-occurrence"); err != nil {
				t.Fatal("remove expired occurrence fixture:", err)
			}
			jobID := contract.NewID()
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `INSERT INTO _kora_analytics_rebuild_job (id,site,doctype,from_date,status,started_at,updated_at) VALUES (?,?,?,'2026-01-01','queued',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`), jobID, "scanner-test", "Invoice"); err != nil {
				t.Fatal("insert queued analytics rebuild job:", err)
			}
			due, err = hasDueTenantWork(t.Context(), database, dialect)
			if err != nil || !due {
				t.Fatalf("hasDueTenantWork for queued analytics rebuild = (%t, %v), want (true, nil)", due, err)
			}
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `INSERT INTO _kora_analytics_rebuild_lease (site,job_id,owner,fencing_token,lease_until) VALUES (?,?,?,1,?)`), "scanner-test", jobID, "active-worker", time.Now().UTC().Add(time.Hour)); err != nil {
				t.Fatal("insert analytics rebuild lease:", err)
			}
			due, err = hasDueTenantWork(t.Context(), database, dialect)
			if err != nil || due {
				t.Fatalf("hasDueTenantWork for actively leased analytics rebuild = (%t, %v), want (false, nil)", due, err)
			}
			workflowSchema, err := workflowsql.ExecutionSchemaStatements(dialect)
			if err != nil {
				t.Fatal("build workflow fixture schema:", err)
			}
			for _, statement := range workflowSchema {
				if _, err := database.ExecContext(t.Context(), statement); err != nil {
					t.Fatalf("create workflow fixture: %v\n%s", err, statement)
				}
			}
			workflowEventID := contract.NewID()
			workflowID := contract.NewID()
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `INSERT INTO _kora_outbox (id,site,event_type,status) VALUES (?,?,?,'published')`), workflowEventID, "scanner-test", "invoice.paid"); err != nil {
				t.Fatal("insert waiting workflow signal event:", err)
			}
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `INSERT INTO _kora_workflow_instance (id,tenant_id,definition_id,definition_version,subject_kind,subject_id,subject_revision,status,current_step,revision,created_at,updated_at) VALUES (?,?,?,1,?,?,?,'waiting',?,1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`), workflowID, "scanner-test", "workflow-def", "invoice", "invoice-1", "rev-1", "wait-payment"); err != nil {
				t.Fatal("insert waiting workflow instance:", err)
			}
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `INSERT INTO _kora_workflow_subscription (tenant_id,event_type,instance_id,step_id,correlation_field,correlation_value,on_event) VALUES (?,?,?,?,'','',?)`), "scanner-test", "invoice.paid", workflowID, "wait-payment", "next-step"); err != nil {
				t.Fatal("insert workflow event subscription:", err)
			}
			due, err = hasDueTenantWork(t.Context(), database, dialect)
			if err != nil || !due {
				t.Fatalf("hasDueTenantWork for unreceipted waiting workflow signal = (%t, %v), want (true, nil)", due, err)
			}
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `INSERT INTO _kora_outbox_receipt (consumer_name,event_id,site) VALUES (?,?,?)`), "workflow-signals", workflowEventID, "scanner-test"); err != nil {
				t.Fatal("insert workflow signal receipt:", err)
			}
			due, err = hasDueTenantWork(t.Context(), database, dialect)
			if err != nil || due {
				t.Fatalf("hasDueTenantWork after workflow signal receipt = (%t, %v), want (false, nil)", due, err)
			}
			timerInstance := contract.NewID()
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `INSERT INTO _kora_workflow_process_timer (tenant_id,instance_id,step_id,generation,due_at) VALUES (?,?,?,1,CURRENT_TIMESTAMP)`), "scanner-test", timerInstance, "wait-timer"); err != nil {
				t.Fatal("insert due workflow timer:", err)
			}
			due, err = hasDueTenantWork(t.Context(), database, dialect)
			if err != nil || !due {
				t.Fatalf("hasDueTenantWork for due workflow timer = (%t, %v), want (true, nil)", due, err)
			}
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `DELETE FROM _kora_workflow_process_timer WHERE instance_id=?`), timerInstance); err != nil {
				t.Fatal("remove workflow timer fixture:", err)
			}
			requestSchema, err := requestsql.SchemaStatements(dialect)
			if err != nil {
				t.Fatal("build request fixture schema:", err)
			}
			for _, statement := range requestSchema {
				if _, err := database.ExecContext(t.Context(), statement); err != nil {
					t.Fatalf("create request fixture: %v\n%s", err, statement)
				}
			}
			requestID := contract.NewID()
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `INSERT INTO _kora_request (id,tenant_id,revision,kind,status,requester_id,title,message,due_at,created_at,updated_at) VALUES (?,?,1,'approval','pending','requester','title','message',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`), requestID, "scanner-test"); err != nil {
				t.Fatal("insert due request:", err)
			}
			due, err = hasDueTenantWork(t.Context(), database, dialect)
			if err != nil || !due {
				t.Fatalf("hasDueTenantWork for due request = (%t, %v), want (true, nil)", due, err)
			}
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `DELETE FROM _kora_request WHERE id=?`), requestID); err != nil {
				t.Fatal("remove request fixture:", err)
			}
			notificationSchema, err := notificationsql.SchemaStatements(dialect)
			if err != nil {
				t.Fatal("build notification fixture schema:", err)
			}
			for _, statement := range notificationSchema {
				if _, err := database.ExecContext(t.Context(), statement); err != nil {
					t.Fatalf("create notification fixture: %v\n%s", err, statement)
				}
			}
			deliveryID := contract.NewID()
			if _, err := database.ExecContext(t.Context(), db.Rebind(dialect, `INSERT INTO _kora_notification_delivery (id,tenant_id,recipient_id,request_id,channel,event_id,status,available_at,created_at,updated_at) VALUES (?,?,?,?,'in_app',?,'pending',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`), deliveryID, "scanner-test", "recipient", "request", contract.NewEventID()); err != nil {
				t.Fatal("insert due notification delivery:", err)
			}
			due, err = hasDueTenantWork(t.Context(), database, dialect)
			if err != nil || !due {
				t.Fatalf("hasDueTenantWork for due notification delivery = (%t, %v), want (true, nil)", due, err)
			}
		})
	}
}

func analyticsScannerSchema(driver string) []string {
	identity := "VARCHAR(140)"
	timestamp := "DATETIME(6)"
	if driver == "postgres" {
		identity, timestamp = "TEXT", "TIMESTAMPTZ"
	}
	if driver == "libsql" {
		identity, timestamp = "TEXT", "TEXT"
	}
	engine := ""
	if driver == "mysql" {
		engine = " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci"
	}
	return []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS _kora_analytics_rebuild_job (id %s PRIMARY KEY, site %s NOT NULL, doctype %s NOT NULL, from_date %s NOT NULL, status %s NOT NULL, started_at %s NOT NULL, updated_at %s NOT NULL)%s`, identity, identity, identity, identity, identity, timestamp, timestamp, engine),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS _kora_analytics_rebuild_lease (site %s NOT NULL, job_id %s NOT NULL, owner %s NOT NULL, fencing_token BIGINT NOT NULL, lease_until %s NOT NULL, PRIMARY KEY(site,job_id))%s`, identity, identity, identity, timestamp, engine),
	}
}

func outboxSchema(driver string) []string {
	switch driver {
	case "mysql":
		return db.OutboxTablesMySQL()
	case "postgres":
		return db.OutboxTablesPostgres()
	case "libsql":
		return db.OutboxTablesLibSQL()
	default:
		panic(fmt.Sprintf("unsupported fixture dialect %q", driver))
	}
}
