package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/db"
)

func TestPublisherMetrics(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer database.Close()

	mock.ExpectQuery(`(?s)SELECT\s+COALESCE\(SUM\(CASE WHEN status = 'pending'.*FROM _kora_outbox`).
		WillReturnRows(sqlmock.NewRows([]string{"pending", "failed", "published"}).AddRow(3, 1, 9))

	admission := &dispatcherAdmissionProbe{}
	p := &Publisher{DB: database, Dialect: db.Resolve("mysql"), WorkAdmission: admission}
	m, err := p.Metrics(context.Background())
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if m.Pending != 3 || m.Failed != 1 || m.Published != 9 {
		t.Fatalf("metrics = %+v, want pending=3 failed=1 published=9", m)
	}
	if admission.calls.Load() != 1 || admission.active.Load() != 0 {
		t.Fatalf("metrics admission calls/active = %d/%d, want 1/0", admission.calls.Load(), admission.active.Load())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPublisherBacklogUsesBoundedIndexedStateSamples(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock.New:", err)
	}
	defer database.Close()
	oldestPending := time.Now().Add(-2 * time.Minute)
	rows := sqlmock.NewRows([]string{"created_at"})
	for i := 0; i < BacklogSampleLimit; i++ {
		rows.AddRow(oldestPending.Add(time.Duration(i) * time.Millisecond))
	}
	mock.ExpectQuery(`SELECT created_at FROM _kora_outbox WHERE status = \? ORDER BY created_at ASC LIMIT \?`).
		WithArgs("pending", BacklogSampleLimit).WillReturnRows(rows)
	mock.ExpectQuery(`SELECT created_at FROM _kora_outbox WHERE status = \? ORDER BY created_at ASC LIMIT \?`).
		WithArgs("failed", BacklogSampleLimit).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(time.Now().Add(-30 * time.Second)))
	mock.ExpectQuery(`SELECT created_at FROM _kora_outbox WHERE status = \? ORDER BY created_at ASC LIMIT \?`).
		WithArgs("publishing", BacklogSampleLimit).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}))
	admission := &dispatcherAdmissionProbe{}
	metrics, err := (&Publisher{DB: database, Dialect: db.Resolve("mysql"), WorkAdmission: admission}).Backlog(context.Background())
	if err != nil {
		t.Fatalf("Backlog: %v", err)
	}
	if metrics.Pending != BacklogSampleLimit || metrics.Failed != 1 || metrics.Publishing != 0 || !metrics.Saturated {
		t.Fatalf("bounded backlog sample = %+v", metrics)
	}
	if metrics.OldestUnpublishedAge < time.Minute {
		t.Fatalf("oldest sampled event age = %s, want >=1m", metrics.OldestUnpublishedAge)
	}
	if admission.calls.Load() != 3 || admission.active.Load() != 0 {
		t.Fatalf("backlog admission calls/active = %d/%d, want three SQL samples and no held permit", admission.calls.Load(), admission.active.Load())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestBacklogMetricsKeepLeaseLossAcrossRefresh(t *testing.T) {
	metrics := &BacklogMetrics{}
	metrics.RecordLeaseLost()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock.New:", err)
	}
	defer database.Close()
	for _, state := range []string{"pending", "failed", "publishing"} {
		mock.ExpectQuery(`SELECT created_at FROM _kora_outbox WHERE status = \? ORDER BY created_at ASC LIMIT \?`).
			WithArgs(state, BacklogSampleLimit).
			WillReturnRows(sqlmock.NewRows([]string{"created_at"}))
	}
	if err := metrics.Refresh(context.Background(), &Publisher{DB: database, Dialect: db.Resolve("mysql")}); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := metrics.Snapshot(time.Now()).LeaseLost; got != 1 {
		t.Fatalf("lease lost snapshot = %d, want 1 after successful refresh", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
