package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/site"
)

type rejectedDirectoryAdmission struct{ called chan struct{} }

func (a rejectedDirectoryAdmission) Acquire(context.Context) (func(), error) {
	a.called <- struct{}{}
	return nil, dbaccess.ErrExhausted
}

func TestDirectoryLagObservationDefersPlatformQueryWithoutAdmission(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	registry := site.NewSQLSiteRegistry(database, "mysql")
	called := make(chan struct{}, 1)
	observeDirectoryLag(context.Background(), registry, "engine:test", nil, rejectedDirectoryAdmission{called: called})
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("directory lag observation did not attempt work admission")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("directory query ran without admission: %v", err)
	}
}

var _ interface {
	ConsumerLagContext(context.Context, string) (site.DirectoryConsumerLag, error)
} = (*site.SQLSiteRegistry)(nil)

func TestDirectoryLagMonitorJoinsObserver(t *testing.T) {
	started := make(chan struct{})
	monitor, err := startDirectoryLagMonitor(context.Background(), func(ctx context.Context) {
		close(started)
		<-ctx.Done()
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := monitor.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestDirectoryLagMonitorTimeoutCanBeRetried(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	monitor, err := startDirectoryLagMonitor(context.Background(), func(context.Context) {
		close(started)
		<-release
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := monitor.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Shutdown error = %v, want deadline", err)
	}
	close(release)
	if err := monitor.Shutdown(context.Background()); err != nil {
		t.Fatalf("retry Shutdown after observer exits: %v", err)
	}
}
