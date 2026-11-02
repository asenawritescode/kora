package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/scheduler"
)

func TestScheduleSchemaCoversSupportedDialects(t *testing.T) {
	for _, driver := range []string{"mysql", "postgres", "libsql"} {
		t.Run(driver, func(t *testing.T) {
			statements := tableStatements(driver)
			if len(statements) < 2 {
				t.Fatalf("schema statements = %d, want definition and occurrence tables", len(statements))
			}
			for _, table := range []string{"_kora_schedule_definition", "_kora_schedule_occurrence"} {
				found := false
				for _, statement := range statements {
					if regexp.MustCompile(`CREATE TABLE IF NOT EXISTS ` + table).MatchString(statement) {
						found = true
					}
				}
				if !found {
					t.Errorf("%s schema omits %s", driver, table)
				}
			}
			joined := strings.Join(statements, "\n")
			if !strings.Contains(joined, "idx_schedule_pending_backlog") {
				t.Errorf("%s schema omits the tenant pending-backlog index", driver)
			}
		})
	}
}

func TestClaimDueFencesOwnedScheduledWorkAgainstDrainingModule(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, &db.LibSQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	jobJSON, _ := json.Marshal(scheduler.JobConfig{Name: "billing.nightly", OwnerModuleID: "billing.core", Type: "custom", Schedule: "0 1 * * *"})
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT job_json FROM _kora_schedule_definition WHERE tenant_id=? AND job_name=?`)).WithArgs("tenant-a", "billing.nightly").WillReturnRows(sqlmock.NewRows([]string{"job_json"}).AddRow(string(jobJSON)))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_module_snapshot SET revision=revision WHERE tenant_id=?`)).WithArgs("tenant-a").WillReturnResult(sqlmock.NewResult(0, 1))
	snapshot, _ := json.Marshal(contract.ModuleSnapshot{TenantID: "tenant-a", Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: "billing.core", State: string(contract.ModuleDraining), StateChangedAt: time.Now().UTC()}}})
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT snapshot_json FROM _kora_module_snapshot WHERE tenant_id=?`)).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"snapshot_json"}).AddRow(string(snapshot)))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_schedule_occurrence SET state='outcome_unknown',lease_until=NULL,finished_at=STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW'),last_error='lease expired while execution outcome was unknown' WHERE tenant_id=? AND job_name=? AND state='running' AND lease_until<=STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')`)).WithArgs("tenant-a", "billing.nightly").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM _kora_schedule_occurrence WHERE tenant_id=? AND job_name=? AND state='outcome_unknown'`)).WithArgs("tenant-a", "billing.nightly").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM _kora_schedule_occurrence WHERE tenant_id=? AND job_name=? AND state='running' AND lease_until>STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')`)).WithArgs("tenant-a", "billing.nightly").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT scheduled_at,definition_digest,job_json FROM _kora_schedule_occurrence WHERE tenant_id=? AND job_name=? AND state='pending' ORDER BY scheduled_at LIMIT 1`)).WithArgs("tenant-a", "billing.nightly").WillReturnError(sql.ErrNoRows)
	mock.ExpectCommit()
	claimed, err := store.ClaimDue(context.Background(), "tenant-a", "billing.nightly", "worker-a", time.Now().UTC(), time.Minute)
	if err != nil || claimed != nil {
		t.Fatalf("draining module scheduled claim = %+v, %v; want no claim", claimed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestClaimDueCompletesPendingOccurrenceCreatedBeforeDrain(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, &db.LibSQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	tenant, jobName := "tenant-a", "billing.nightly"
	drainedAt := time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)
	scheduledAt := drainedAt.Add(-time.Hour)
	jobJSON, _ := json.Marshal(scheduler.JobConfig{Name: jobName, OwnerModuleID: "billing.core", Type: "custom", Schedule: "0 1 * * *"})
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT job_json FROM _kora_schedule_definition WHERE tenant_id=? AND job_name=?`)).WithArgs(tenant, jobName).WillReturnRows(sqlmock.NewRows([]string{"job_json"}).AddRow(string(jobJSON)))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_module_snapshot SET revision=revision WHERE tenant_id=?`)).WithArgs(tenant).WillReturnResult(sqlmock.NewResult(0, 1))
	snapshot, _ := json.Marshal(contract.ModuleSnapshot{TenantID: contract.TenantID(tenant), Installations: []contract.ModuleInstallation{{TenantID: contract.TenantID(tenant), ModuleID: "billing.core", State: string(contract.ModuleDraining), StateChangedAt: drainedAt}}})
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT snapshot_json FROM _kora_module_snapshot WHERE tenant_id=?`)).WithArgs(tenant).WillReturnRows(sqlmock.NewRows([]string{"snapshot_json"}).AddRow(string(snapshot)))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_schedule_occurrence SET state='outcome_unknown',lease_until=NULL,finished_at=STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW'),last_error='lease expired while execution outcome was unknown' WHERE tenant_id=? AND job_name=? AND state='running' AND lease_until<=STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')`)).WithArgs(tenant, jobName).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM _kora_schedule_occurrence WHERE tenant_id=? AND job_name=? AND state='outcome_unknown'`)).WithArgs(tenant, jobName).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM _kora_schedule_occurrence WHERE tenant_id=? AND job_name=? AND state='running' AND lease_until>STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')`)).WithArgs(tenant, jobName).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	occDigest := strings.Repeat("a", 64)
	pendingJob, _ := json.Marshal(scheduler.JobConfig{Name: jobName, OwnerModuleID: "billing.core", Type: "custom", Schedule: "0 1 * * *"})
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT scheduled_at,definition_digest,job_json FROM _kora_schedule_occurrence WHERE tenant_id=? AND job_name=? AND state='pending' ORDER BY scheduled_at LIMIT 1`)).WithArgs(tenant, jobName).WillReturnRows(sqlmock.NewRows([]string{"scheduled_at", "definition_digest", "job_json"}).AddRow(scheduledAt.Format(time.RFC3339Nano), occDigest, string(pendingJob)))
	stamp := scheduledAt.Format("2006-01-02 15:04:05.000")
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_schedule_occurrence SET state='running',owner=?,fence=fence+1,lease_until=STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW', '+' || (? / 1000000.0) || ' seconds'),attempts=attempts+1 WHERE tenant_id=? AND job_name=? AND scheduled_at=? AND state='pending'`)).WithArgs("worker-a", int64(time.Minute.Microseconds()), tenant, jobName, stamp).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT fence,attempts FROM _kora_schedule_occurrence WHERE tenant_id=? AND job_name=? AND scheduled_at=?`)).WithArgs(tenant, jobName, stamp).WillReturnRows(sqlmock.NewRows([]string{"fence", "attempts"}).AddRow(1, 1))
	mock.ExpectCommit()
	claimed, err := store.ClaimDue(context.Background(), tenant, jobName, "worker-a", drainedAt.Add(time.Minute), time.Minute)
	if err != nil || claimed == nil || !claimed.ScheduledAt.Equal(scheduledAt) {
		t.Fatalf("pre-drain pending occurrence claim = %+v, %v", claimed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestInspectOpenWorkReportsOwnedScheduledOccurrences(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, &db.LibSQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	owned, _ := json.Marshal(scheduler.JobConfig{Name: "billing.nightly", OwnerModuleID: "billing.core"})
	other, _ := json.Marshal(scheduler.JobConfig{Name: "sales.nightly", OwnerModuleID: "sales.core"})
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT job_name,job_json FROM _kora_schedule_definition WHERE tenant_id=? ORDER BY job_name LIMIT 257`)).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"job_name", "job_json"}).AddRow("billing.nightly", string(owned)).AddRow("sales.nightly", string(other)))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM _kora_schedule_occurrence WHERE tenant_id=? AND job_name=? AND state IN ('pending','running','outcome_unknown')`)).WithArgs("tenant-a", "billing.nightly").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT scheduled_at FROM _kora_schedule_occurrence WHERE tenant_id=? AND job_name=? AND state IN ('pending','running','outcome_unknown') ORDER BY scheduled_at LIMIT ?`)).WithArgs("tenant-a", "billing.nightly", 5).WillReturnRows(sqlmock.NewRows([]string{"scheduled_at"}).AddRow("2026-11-01T01:00:00Z").AddRow("2026-11-02T01:00:00Z"))
	blockers, err := store.InspectOpenWork(context.Background(), "tenant-a", "billing.core", 5)
	if err != nil || len(blockers) != 1 || blockers[0].Code != "scheduled_execution_open" || blockers[0].Count != 2 || len(blockers[0].Examples) != 2 {
		t.Fatalf("scheduled blockers = %+v, %v", blockers, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBacklogMetricsScansOldestPendingAcrossDialects(t *testing.T) {
	oldest := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		driver  string
		dialect db.Dialect
		query   string
	}{
		{"mysql", &db.MySQLDialect{}, `SELECT COUNT(*), MIN(scheduled_at) FROM _kora_schedule_occurrence WHERE tenant_id=? AND state='pending'`},
		{"postgres", &db.PostgresDialect{}, `SELECT COUNT(*), MIN(scheduled_at) FROM _kora_schedule_occurrence WHERE tenant_id=$1 AND state='pending'`},
		{"libsql", &db.LibSQLDialect{}, `SELECT COUNT(*), MIN(scheduled_at) FROM _kora_schedule_occurrence WHERE tenant_id=? AND state='pending'`},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			store, err := New(database, tc.dialect)
			if err != nil {
				t.Fatal(err)
			}
			mock.ExpectQuery(regexp.QuoteMeta(tc.query)).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"count", "oldest"}).AddRow(2, oldest))
			count, gotOldest, err := store.BacklogMetrics(context.Background(), "tenant-a")
			if err != nil || count != 2 || !gotOldest.Equal(oldest) {
				t.Fatalf("BacklogMetrics = (%d, %s, %v), want (2, %s, nil)", count, gotOldest, err, oldest)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNextRunAtAcrossDialects(t *testing.T) {
	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		driver  string
		dialect db.Dialect
		query   string
	}{
		{"mysql", &db.MySQLDialect{}, `SELECT next_run_at FROM _kora_schedule_definition WHERE tenant_id=? AND job_name=?`},
		{"postgres", &db.PostgresDialect{}, `SELECT next_run_at FROM _kora_schedule_definition WHERE tenant_id=$1 AND job_name=$2`},
		{"libsql", &db.LibSQLDialect{}, `SELECT next_run_at FROM _kora_schedule_definition WHERE tenant_id=? AND job_name=?`},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			store, err := New(database, tc.dialect)
			if err != nil {
				t.Fatal(err)
			}
			mock.ExpectQuery(regexp.QuoteMeta(tc.query)).WithArgs("tenant-a", "hourly").
				WillReturnRows(sqlmock.NewRows([]string{"next_run_at"}).AddRow(want))
			got, err := store.NextRunAt(context.Background(), "tenant-a", "hourly")
			if err != nil || !got.Equal(want) {
				t.Fatalf("NextRunAt = (%s, %v), want (%s, nil)", got, err, want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEnsureAddsBacklogIndexToExistingMySQLTable(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, &db.MySQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range tableStatements("mysql") {
		mock.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='_kora_schedule_occurrence' AND index_name='idx_schedule_pending_backlog'`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(regexp.QuoteMeta(`CREATE INDEX idx_schedule_pending_backlog ON _kora_schedule_occurrence (tenant_id, state, scheduled_at)`)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure existing MySQL scheduler tables: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterDefinitionKeepsCursorForSameDigest(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, &db.MySQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	job := &scheduler.JobConfig{Name: "hourly", Type: scheduler.JobWebhook, Schedule: "0 * * * *"}
	digest := scheduler.JobDefinitionDigest(job)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT definition_digest FROM _kora_schedule_definition WHERE tenant_id=? AND job_name=? FOR UPDATE`)).WithArgs("tenant-a", "hourly").WillReturnRows(sqlmock.NewRows([]string{"definition_digest"}).AddRow(digest))
	mock.ExpectCommit()
	if err := store.RegisterDefinition(context.Background(), "tenant-a", job, digest, time.Now().Add(time.Hour), time.Now()); err != nil {
		t.Fatal("register unchanged schedule:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterOwnerScheduleRejectedWhileModuleDrains(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, &db.MySQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	job := &scheduler.JobConfig{Name: "billing-run", OwnerModuleID: "billing.core", Type: scheduler.JobWebhook, Schedule: "0 * * * *"}
	digest := scheduler.JobDefinitionDigest(job)
	mock.ExpectBegin()
	definitionQuery := regexp.QuoteMeta(`SELECT definition_digest FROM _kora_schedule_definition WHERE tenant_id=? AND job_name=? FOR UPDATE`)
	mock.ExpectQuery(definitionQuery).WithArgs("tenant-a", "billing-run").WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_module_snapshot SET revision=revision WHERE tenant_id=?")).WithArgs("tenant-a").WillReturnResult(sqlmock.NewResult(0, 1))
	snapshot, _ := json.Marshal(contract.ModuleSnapshot{TenantID: "tenant-a", Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: "billing.core", State: string(contract.ModuleDraining)}}})
	mock.ExpectQuery(regexp.QuoteMeta("SELECT snapshot_json FROM _kora_module_snapshot WHERE tenant_id=?")).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"snapshot_json"}).AddRow(string(snapshot)))
	err = store.RegisterDefinition(context.Background(), "tenant-a", job, digest, time.Now().Add(time.Hour), time.Now())
	var domainErr *contract.Error
	if !errors.As(err, &domainErr) || domainErr.Type != contract.CodePermissionDenied {
		t.Fatalf("register scheduled definition error=%v, want permission denied", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDisabledOwnerScheduleReloadDoesNotResumeClaims(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, &db.LibSQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	job := &scheduler.JobConfig{Name: "billing-run", OwnerModuleID: "billing.core", Type: scheduler.JobWebhook, Schedule: "0 * * * *"}
	digest := scheduler.JobDefinitionDigest(job)
	jobJSON, _ := json.Marshal(job)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT definition_digest FROM _kora_schedule_definition WHERE tenant_id=? AND job_name=?`)).WithArgs("tenant-a", job.Name).WillReturnRows(sqlmock.NewRows([]string{"definition_digest"}).AddRow(digest))
	mock.ExpectCommit()
	if err := store.RegisterDefinition(context.Background(), "tenant-a", job, digest, time.Now().Add(time.Hour), time.Now()); err != nil {
		t.Fatalf("reload unchanged disabled schedule: %v", err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT job_json FROM _kora_schedule_definition WHERE tenant_id=? AND job_name=?`)).WithArgs("tenant-a", job.Name).WillReturnRows(sqlmock.NewRows([]string{"job_json"}).AddRow(string(jobJSON)))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_module_snapshot SET revision=revision WHERE tenant_id=?`)).WithArgs("tenant-a").WillReturnResult(sqlmock.NewResult(0, 1))
	snapshot, _ := json.Marshal(contract.ModuleSnapshot{TenantID: "tenant-a", Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: "billing.core", State: string(contract.ModuleDisabled)}}})
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT snapshot_json FROM _kora_module_snapshot WHERE tenant_id=?`)).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"snapshot_json"}).AddRow(string(snapshot)))
	mock.ExpectCommit()
	claimed, err := store.ClaimDue(context.Background(), "tenant-a", job.Name, "worker-a", time.Now().UTC(), time.Minute)
	if err != nil || claimed != nil {
		t.Fatalf("disabled owner schedule claim = %+v, %v; want no claim", claimed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterDefinitionRecoversConcurrentFirstInsert(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, &db.MySQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	job := &scheduler.JobConfig{Name: "hourly", Type: scheduler.JobWebhook, Schedule: "0 * * * *"}
	digest := scheduler.JobDefinitionDigest(job)
	mock.ExpectBegin()
	query := regexp.QuoteMeta(`SELECT definition_digest FROM _kora_schedule_definition WHERE tenant_id=? AND job_name=? FOR UPDATE`)
	mock.ExpectQuery(query).WithArgs("tenant-a", "hourly").WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta(`INSERT IGNORE INTO _kora_schedule_definition (tenant_id,job_name,definition_digest,schedule,job_json,next_run_at,updated_at) VALUES (?,?,?,?,?,?,?)`)).WithArgs("tenant-a", "hourly", digest, job.Schedule, `{"name":"hourly","type":"webhook","schedule":"0 * * * *","config":null}`, sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(query).WithArgs("tenant-a", "hourly").WillReturnRows(sqlmock.NewRows([]string{"definition_digest"}).AddRow(digest))
	mock.ExpectCommit()
	if err := store.RegisterDefinition(context.Background(), "tenant-a", job, digest, time.Now(), time.Now()); err != nil {
		t.Fatal("register schedule after concurrent insert:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterDefinitionRejectsBadDigestWithoutDatabaseWork(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, &db.MySQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	job := &scheduler.JobConfig{Name: "hourly", Schedule: "* * * * *"}
	if err := store.RegisterDefinition(context.Background(), "tenant-a", job, "wrong", time.Now(), time.Now()); err == nil {
		t.Fatal("invalid digest unexpectedly accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
