package sqlstore

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
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
