package engine

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/site"
)

func TestPendingDeletionWorkerWaitsUntilStartAndJoins(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	called := make(chan struct{}, 1)
	worker, err := startPendingDeletionWorker(context.Background(), pendingDeletionRecovery{platformDB: database, dbType: "mysql", retry: func(ctx context.Context, _ *sql.DB, _ string, _ *site.CommonConfig) error {
		called <- struct{}{}
		<-ctx.Done()
		return nil
	}, interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
		t.Fatal("deletion recovery ran before readiness released it")
	case <-time.After(20 * time.Millisecond):
	}

	worker.Start()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("deletion recovery did not start after readiness")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := worker.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

type deletionAdmission struct {
	err    error
	called chan struct{}
}

func (a *deletionAdmission) Acquire(context.Context) (func(), error) {
	if a.called != nil {
		a.called <- struct{}{}
	}
	if a.err != nil {
		return nil, a.err
	}
	return func() {}, nil
}

func TestPendingDeletionWorkerRetriesAdmissionRejection(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	admission := &deletionAdmission{err: dbaccess.ErrExhausted, called: make(chan struct{}, 1)}
	called := make(chan struct{}, 1)
	worker, err := startPendingDeletionWorker(context.Background(), pendingDeletionRecovery{platformDB: database, dbType: "mysql", retry: func(context.Context, *sql.DB, string, *site.CommonConfig) error {
		called <- struct{}{}
		return nil
	}, interval: time.Hour, admission: admission})
	if err != nil {
		t.Fatal(err)
	}
	worker.Start()
	select {
	case <-admission.called:
	case <-time.After(time.Second):
		t.Fatal("deletion worker did not request work admission")
	}
	select {
	case <-called:
		t.Fatal("deletion retry ran without work admission")
	case <-time.After(20 * time.Millisecond):
	}
	if snapshot := worker.Snapshot(); snapshot.AdmissionRejections != 1 || snapshot.Attempts != 0 || snapshot.Failures != 0 {
		t.Fatalf("admission-rejected worker snapshot = %+v", snapshot)
	}
	if err := worker.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPendingDeletionWorkerCanStopBeforeReadiness(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	called := make(chan struct{}, 1)
	worker, err := startPendingDeletionWorker(context.Background(), pendingDeletionRecovery{platformDB: database, dbType: "mysql", retry: func(context.Context, *sql.DB, string, *site.CommonConfig) error {
		called <- struct{}{}
		return nil
	}, interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := worker.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown before Start: %v", err)
	}
	select {
	case <-called:
		t.Fatal("deletion recovery ran before readiness")
	default:
	}
}

func TestPendingDeletionWorkerCountsRetryFailures(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	called := make(chan struct{}, 1)
	worker, err := startPendingDeletionWorker(context.Background(), pendingDeletionRecovery{platformDB: database, dbType: "mysql", retry: func(context.Context, *sql.DB, string, *site.CommonConfig) error {
		called <- struct{}{}
		return errors.New("transient delete failure")
	}, interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	worker.Start()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("deletion retry did not run")
	}
	deadline := time.Now().Add(time.Second)
	for worker.Snapshot().Failures == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if snapshot := worker.Snapshot(); snapshot.Attempts != 1 || snapshot.Failures != 1 {
		t.Fatalf("failed retry snapshot = %+v", snapshot)
	}
	if err := worker.Shutdown(context.Background()); err != nil {
		t.Fatal("shutdown:", err)
	}
}

func TestStartPendingDeletionWorkerRequiresItsInputs(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	retry := func(context.Context, *sql.DB, string, *site.CommonConfig) error { return nil }
	tests := []struct {
		name   string
		parent context.Context
		rec    pendingDeletionRecovery
	}{
		{"no context", nil, pendingDeletionRecovery{platformDB: database, retry: retry, interval: time.Hour}},
		{"no platform database", context.Background(), pendingDeletionRecovery{retry: retry, interval: time.Hour}},
		{"no retry function", context.Background(), pendingDeletionRecovery{platformDB: database, interval: time.Hour}},
		{"no interval", context.Background(), pendingDeletionRecovery{platformDB: database, retry: retry}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker, err := startPendingDeletionWorker(tt.parent, tt.rec) //nolint:staticcheck // a nil context is the rejected input under test
			if err == nil || worker != nil {
				t.Fatalf("startPendingDeletionWorker = (%v, %v), want an error and no worker", worker, err)
			}
		})
	}
}
