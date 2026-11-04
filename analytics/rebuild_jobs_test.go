package analytics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

func expectRebuildJobCandidate(mock sqlmock.Sqlmock) {
	mock.ExpectQuery("SELECT j.id, j.doctype, j.from_date FROM").WithArgs("tenant").
		WillReturnRows(sqlmock.NewRows([]string{"id", "doctype", "from_date"}).AddRow("job-1", "Invoice", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)))
}

func TestRebuildRunnerMetricsSnapshotReturnsAtomicCounters(t *testing.T) {
	runner := &RebuildRunner{}
	runner.active.Store(1)
	runner.claims.Store(7)
	runner.claimFailures.Store(2)
	runner.admissionDeferrals.Store(3)
	runner.executionFailures.Store(4)
	runner.leaseRenewFailures.Store(5)
	runner.leaseLost.Store(6)
	runner.completionFailures.Store(7)
	want := RebuildMetricsSnapshot{Active: 1, Claims: 7, ClaimFailures: 2, AdmissionDeferrals: 3,
		ExecutionFailures: 4, LeaseRenewFailures: 5, LeaseLost: 6, CompletionFailures: 7}
	if got := runner.MetricsSnapshot(); got != want {
		t.Fatalf("rebuild metrics snapshot = %+v, want %+v", got, want)
	}
	if got := (*RebuildRunner)(nil).MetricsSnapshot(); got != (RebuildMetricsSnapshot{}) {
		t.Fatalf("nil rebuild metrics snapshot = %+v, want zero", got)
	}
}

func TestRebuildRunnerCountsClaimFailuresAndStops(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create rebuild runner DB:", err)
	}
	defer database.Close()
	mock.ExpectQuery("SELECT j.id, j.doctype, j.from_date FROM").WithArgs("tenant").
		WillReturnError(errors.New("temporary database failure"))
	runner := NewRebuildRunner(database, db.Resolve("mysql"), "tenant", doctype.NewRegistry(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runner.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for runner.MetricsSnapshot().ClaimFailures == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := runner.MetricsSnapshot().ClaimFailures; got != 1 {
		cancel()
		<-done
		t.Fatalf("claim failures = %d, want 1", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("rebuild runner did not stop after cancellation")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("rebuild runner SQL expectation:", err)
	}
}

func TestClaimNextRebuildJobCreatesFencedLease(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	from := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("SELECT j.id, j.doctype, j.from_date FROM").WithArgs("tenant").
		WillReturnRows(sqlmock.NewRows([]string{"id", "doctype", "from_date"}).AddRow("job-1", "Invoice", from))
	mock.ExpectBegin()
	mock.ExpectExec("INSERT IGNORE INTO .*_kora_analytics_rebuild_lease.*VALUES \\(\\?, \\?, \\?, 1, DATE_ADD\\(CURRENT_TIMESTAMP, INTERVAL 45 SECOND\\)\\)").
		WithArgs("tenant", "job-1", "runner-a").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT fencing_token FROM .*_kora_analytics_rebuild_lease").
		WithArgs("tenant", "job-1", "runner-a").WillReturnRows(sqlmock.NewRows([]string{"fencing_token"}).AddRow(1))
	mock.ExpectExec("UPDATE .*_kora_analytics_rebuild_job.*SET status = 'running'").
		WithArgs(sqlmock.AnyArg(), "tenant", "job-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	job, err := ClaimNextRebuildJob(context.Background(), database, db.Resolve("mysql"), "tenant", "runner-a")
	if err != nil || job == nil {
		t.Fatalf("ClaimNextRebuildJob() = (%v, %v), want claimed job", job, err)
	}
	if job.ID != "job-1" || job.DocType != "Invoice" || !job.From.Equal(from) || job.FencingToken != 1 || job.LeaseOwner != "runner-a" {
		t.Fatalf("claimed job = %#v", job)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestClaimNextRebuildJobDoesNotTakeLiveLease(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	expectRebuildJobCandidate(mock)
	mock.ExpectBegin()
	mock.ExpectExec("INSERT IGNORE INTO .*_kora_analytics_rebuild_lease").
		WithArgs("tenant", "job-1", "runner-b").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("UPDATE .*_kora_analytics_rebuild_lease.*lease_until <= CURRENT_TIMESTAMP").
		WithArgs("runner-b", "tenant", "job-1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()
	job, err := ClaimNextRebuildJob(context.Background(), database, db.Resolve("mysql"), "tenant", "runner-b")
	if err != nil || job != nil {
		t.Fatalf("ClaimNextRebuildJob() = (%v, %v), want unclaimed job", job, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestClaimNextRebuildJobPropagatesCanceledContext(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mock.ExpectQuery("SELECT j.id, j.doctype, j.from_date FROM").WithArgs("tenant").WillReturnError(context.Canceled)
	_, err = ClaimNextRebuildJob(ctx, database, db.Resolve("mysql"), "tenant", "runner-a")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ClaimNextRebuildJob error = %v, want context.Canceled", err)
	}
}

type rebuildAdmissionProbe struct {
	ordinary int
	heavy    int
	released int
}

func (a *rebuildAdmissionProbe) Acquire(context.Context) (func(), error) {
	a.ordinary++
	return func() { a.released++ }, nil
}

func (a *rebuildAdmissionProbe) AcquireMaintenance(context.Context) (func(), error) {
	a.heavy++
	return func() { a.released++ }, nil
}

type rejectingRebuildAdmission struct{ err error }

func (a rejectingRebuildAdmission) Acquire(context.Context) (func(), error) {
	return nil, a.err
}
func (a rejectingRebuildAdmission) AcquireMaintenance(context.Context) (func(), error) {
	return nil, a.err
}

func TestRebuildAdmissionRejectionDoesNotBypassBudgetToReleaseLease(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := NewRebuildRunner(database, db.Resolve("mysql"), "tenant", nil, rejectingRebuildAdmission{err: errors.New("maintenance lane full")})
	runner.process(ctx, &RebuildJob{ID: "job-1", Site: "tenant", LeaseOwner: "runner-a", FencingToken: 3})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("admission rejection issued unadmitted SQL: %v", err)
	}
}

func TestRebuildClaimUsesOrdinaryAdmission(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectQuery("SELECT j.id, j.doctype, j.from_date FROM").WithArgs("tenant").
		WillReturnRows(sqlmock.NewRows([]string{"id", "doctype", "from_date"}))
	admission := &rebuildAdmissionProbe{}
	runner := NewRebuildRunner(database, db.Resolve("mysql"), "tenant", nil, admission)
	job, err := runner.claim(context.Background())
	if err != nil || job != nil {
		t.Fatalf("claim() = (%v, %v), want no queued job", job, err)
	}
	if admission.ordinary != 1 || admission.heavy != 0 || admission.released != 1 {
		t.Fatalf("claim admission usage = %#v, want one released ordinary permit", admission)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRebuildLeaseDeadlineExpressionsCoverSupportedDialects(t *testing.T) {
	for _, test := range []struct{ driver, want string }{
		{"mysql", "DATE_ADD(CURRENT_TIMESTAMP, INTERVAL 45 SECOND)"},
		{"postgres", "CURRENT_TIMESTAMP + INTERVAL '45 seconds'"},
		{"libsql", "datetime(CURRENT_TIMESTAMP, '+45 seconds')"},
	} {
		t.Run(test.driver, func(t *testing.T) {
			if got := rebuildLeaseDeadlineExpression(db.Resolve(test.driver)); got != test.want {
				t.Fatalf("lease deadline expression = %q, want %q", got, test.want)
			}
		})
	}
}

func TestStaleRebuildRunnerCannotCompleteJob(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	job := &RebuildJob{ID: "job-1", Site: "tenant", LeaseOwner: "runner-old", FencingToken: 1}
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE .*_kora_analytics_rebuild_job.*EXISTS \\(SELECT 1 FROM .*_kora_analytics_rebuild_lease").
		WithArgs("completed", sqlmock.AnyArg(), 2, sqlmock.AnyArg(), "tenant", "job-1", "tenant", "job-1", "runner-old", int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()
	if err := finishClaimedRebuild(context.Background(), database, db.Resolve("mysql"), job, "completed", 2, ""); err != ErrRebuildLeaseLost {
		t.Fatalf("stale completion error = %v, want ErrRebuildLeaseLost", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRenewRebuildLeaseRejectsStaleFence(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	job := &RebuildJob{ID: "job-1", Site: "tenant", LeaseOwner: "runner-old", FencingToken: 1}
	mock.ExpectExec("UPDATE .*_kora_analytics_rebuild_lease.*SET lease_until = DATE_ADD.*fencing_token = \\?.*lease_until > CURRENT_TIMESTAMP").
		WithArgs("tenant", "job-1", "runner-old", int64(1)).WillReturnResult(sqlmock.NewResult(0, 0))
	if err := renewRebuildLease(context.Background(), database, db.Resolve("mysql"), job); err != ErrRebuildLeaseLost {
		t.Fatalf("stale renewal error = %v, want ErrRebuildLeaseLost", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestParseRebuildDateAcceptsSupportedDriverValues(t *testing.T) {
	want := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		value any
	}{
		{name: "time", value: time.Date(2026, 10, 14, 0, 0, 0, 0, time.FixedZone("db", 3*60*60))},
		{name: "string", value: "2026-10-14"},
		{name: "bytes", value: []byte("2026-10-14")},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseRebuildDate(test.value)
			if err != nil || !got.Equal(want) {
				t.Fatalf("parseRebuildDate(%T) = (%s, %v), want %s", test.value, got, err, want)
			}
		})
	}
	if _, err := parseRebuildDate("not-a-date"); err == nil {
		t.Fatal("malformed SQL date accepted")
	}
}
