package engine

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/analytics"
	"github.com/asenawritescode/kora/internal/auth"
	"github.com/asenawritescode/kora/internal/dbaccess"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
)

func TestStartupSiteCollectionClosesFanoutBusWhenSiblingLoadFails(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal("create startup rollback DB:", err)
	}
	defer database.Close()
	base := analytics.NewChannelBus(4, t.TempDir()+"/wal")
	fanout, err := analytics.NewMultiBus(base)
	if err != nil {
		t.Fatal(err)
	}
	worker := &startupRollbackWorker{stopped: make(chan struct{})}
	loaded := &knet.LoadedSite{
		Name:              "loaded.example",
		DB:                database,
		SessionManager:    auth.NewOwnedSessionManager(database),
		AnalyticsEventBus: fanout,
		AnalyticsWorker:   worker,
	}
	tenantWorkerDone := make(chan struct{})
	if !loaded.StartWorker(context.Background(), func(ctx context.Context) { <-ctx.Done(); close(tenantWorkerDone) }) {
		t.Fatal("tenant worker did not start")
	}
	wantErr := errors.New("sibling load failed")
	_, _, _, err = collectLoadedStartupTenants([]startupTenantResult{
		{name: loaded.Name, site: loaded},
		{name: "failed.example", err: wantErr},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("collection error = %v, want sibling failure", err)
	}
	if err := base.Publish(analytics.ChangeEvent{ID: "after-cleanup", Site: "loaded.example"}); err == nil {
		t.Fatal("startup rollback left the tenant event bus open")
	}
	select {
	case <-worker.stopped:
	case <-time.After(time.Second):
		t.Fatal("startup rollback did not stop its analytics worker")
	}
	select {
	case <-tenantWorkerDone:
	case <-time.After(time.Second):
		t.Fatal("startup rollback did not cancel and join the tenant worker")
	}
	if err := database.Ping(); err == nil {
		t.Fatal("startup rollback left the tenant database pool open")
	}
}

func TestStartupSiteCollectionJoinsRollbackCloseErrors(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create startup rollback DB:", err)
	}
	startupErr := errors.New("sibling load failed")
	closeErr := errors.New("tenant pool close failed")
	mock.ExpectClose().WillReturnError(closeErr)
	_, _, _, err = collectLoadedStartupTenants([]startupTenantResult{
		{name: "loaded.example", site: &knet.LoadedSite{Name: "loaded.example", DB: database}},
		{name: "failed.example", err: startupErr},
	})
	if !errors.Is(err, startupErr) || !errors.Is(err, closeErr) {
		t.Fatalf("collection error = %v, want both startup and rollback close errors", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

type startupRollbackWorker struct{ stopped chan struct{} }

func (w *startupRollbackWorker) StartAsyncContext(context.Context) {}
func (w *startupRollbackWorker) Stop()                             { close(w.stopped) }
func (w *startupRollbackWorker) InvalidateMetrics(string)          {}
func (w *startupRollbackWorker) InvalidateAllMetrics()             {}
func (w *startupRollbackWorker) Flush() error                      { return nil }

type startupTestBackend struct{ closed int }

func (*startupTestBackend) Put(context.Context, string, io.Reader, int64, storage.FileMeta) (*storage.FileMeta, error) {
	return nil, nil
}
func (*startupTestBackend) EnsureBucket(context.Context) error { return nil }
func (*startupTestBackend) Head(context.Context, string) (*storage.FileMeta, error) {
	return nil, nil
}
func (*startupTestBackend) Open(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return nil, nil
}
func (*startupTestBackend) Delete(context.Context, string) error { return nil }
func (*startupTestBackend) URL(context.Context, string) (string, error) {
	return "", nil
}
func (b *startupTestBackend) Close() error {
	b.closed++
	return nil
}

func TestStartupTenantAcquisitionFailuresCloseStorage(t *testing.T) {
	stageErr := errors.New("injected startup stage failure")
	tests := []struct {
		name       string
		resolveErr error
		admitErr   error
		loadErr    error
	}{
		{name: "storage resolution", resolveErr: stageErr},
		{name: "work admission", admitErr: stageErr},
		{name: "tenant activation", loadErr: stageErr},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &startupTestBackend{}
			_, err := loadStartupTenantSet(context.Background(), StartupTenantConfig{
				Sites: []site.DBSiteInfo{{Name: "tenant.example"}}, Common: &site.CommonConfig{},
				BinaryVersion: "test", Concurrency: 1,
				ResolveStorage: func(*site.Config) (storage.Backend, error) {
					return backend, test.resolveErr
				},
			}, startupTenantDependencies{
				admission: func(*site.Config) (*dbaccess.Admission, error) { return nil, test.admitErr },
				loadRuntime: func(context.Context, site.DBSiteInfo, *site.CommonConfig, string) (*knet.LoadedSite, error) {
					return nil, test.loadErr
				},
			})
			if !errors.Is(err, stageErr) {
				t.Fatalf("startup error = %v, want injected stage error", err)
			}
			if backend.closed != 1 {
				t.Fatalf("storage close count = %d, want 1", backend.closed)
			}
		})
	}
}
