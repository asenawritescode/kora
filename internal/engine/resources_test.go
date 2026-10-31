package engine

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/script"
	"github.com/asenawritescode/kora/site"
)

type testScriptRunner struct {
	closed      atomic.Int32
	beforeClose func() error
}

type testNATSCloser struct {
	closed  atomic.Int32
	onClose func()
}

func (c *testNATSCloser) Close() {
	c.closed.Add(1)
	if c.onClose != nil {
		c.onClose()
	}
}

func (*testScriptRunner) Execute(context.Context, script.ExecuteRequest) (*script.ExecuteResult, error) {
	return nil, nil
}
func (*testScriptRunner) Validate(string) error { return nil }
func (r *testScriptRunner) Close() error {
	r.closed.Add(1)
	if r.beforeClose != nil {
		return r.beforeClose()
	}
	return nil
}

func TestProcessResourcesClosesRealtimeStreamsBeforeJoiningTenantWorkers(t *testing.T) {
	streamsClosed := false
	resources := &ProcessResources{
		CloseRealtimeStreams: func() { streamsClosed = true },
		StopAsyncHookWorker: func() {
			if !streamsClosed {
				t.Error("async worker shutdown began before hijacked realtime streams were closed")
			}
		},
	}
	if err := resources.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown process resources: %v", err)
	}
	if !streamsClosed {
		t.Fatal("process shutdown did not close realtime streams")
	}
}

func TestProcessResourcesRetainDependenciesWhenWorkerJoinTimesOut(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	worker, err := startPendingDeletionWorker(context.Background(), database, "mysql", nil, func(context.Context, *sql.DB, string, *site.CommonConfig) error {
		close(entered)
		<-release
		return nil
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	worker.Start()
	<-entered
	runner := &testScriptRunner{}
	resources := &ProcessResources{
		DeletionWorker: worker,
		ScriptRunner:   runner,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := resources.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want deadline", err)
	}
	if got := runner.closed.Load(); got != 0 {
		t.Fatalf("script runner closed %d times while deletion worker may still use it", got)
	}
	close(release)
	if err := resources.Shutdown(context.Background()); err != nil {
		t.Fatalf("retry Shutdown after worker exits: %v", err)
	}
}

func TestProcessResourcesJoinsWorkersBeforeClosingScriptRunner(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	entered := make(chan struct{})
	worker, err := startPendingDeletionWorker(context.Background(), database, "mysql", nil, func(ctx context.Context, _ *sql.DB, _ string, _ *site.CommonConfig) error {
		close(entered)
		<-ctx.Done()
		return nil
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	worker.Start()
	<-entered
	runner := &testScriptRunner{}
	resources := &ProcessResources{
		DeletionWorker: worker,
		ScriptRunner:   runner,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := resources.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := runner.closed.Load(); got != 1 {
		t.Fatalf("script runner close count = %d, want 1", got)
	}
}

func TestProcessResourcesJoinsManagementWorkerBeforeClosingPlatformDB(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectClose()
	done := make(chan struct{})
	cancelled := make(chan struct{})
	resources := &ProcessResources{
		PlatformDB:           database,
		CancelProcess:        func() { close(cancelled) },
		ManagementWorkerDone: done,
	}
	shutdown := make(chan error, 1)
	go func() { shutdown <- resources.Shutdown(context.Background()) }()
	<-cancelled
	select {
	case err := <-shutdown:
		t.Fatalf("Shutdown returned before management worker joined: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	if err := database.Ping(); err != nil {
		t.Fatalf("platform DB closed while management worker was still active: %v", err)
	}
	close(done)
	if err := <-shutdown; err != nil {
		t.Fatalf("Shutdown after worker exit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("platform DB close expectation: %v", err)
	}
}

func TestProcessResourcesClosesUnregisteredTenantBeforeScriptRunner(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectClose()
	runner := &testScriptRunner{beforeClose: func() error {
		if err := database.Ping(); err == nil {
			return errors.New("tenant database still open before script runner close")
		}
		return nil
	}}
	resources := &ProcessResources{
		UnregisteredTenants: []*knet.LoadedSite{{Name: "startup.example", DB: database}},
		ScriptRunner:        runner,
	}
	if err := resources.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if runner.closed.Load() != 1 {
		t.Fatalf("script runner close count = %d, want 1", runner.closed.Load())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessResourcesCanRetainLateTenantCandidateForShutdown(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectClose()
	runtime := &knet.LoadedSite{Name: "late-candidate.example", DB: database}
	resources := &ProcessResources{}
	resources.RetainUnregisteredTenant(runtime)
	resources.RetainUnregisteredTenant(runtime)
	if err := resources.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessResourcesRetainsScriptRunnerWhenUnregisteredTenantCloseFails(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	closeErr := errors.New("tenant database close failed")
	mock.ExpectClose().WillReturnError(closeErr)
	runner := &testScriptRunner{}
	resources := &ProcessResources{
		UnregisteredTenants: []*knet.LoadedSite{{Name: "startup.example", DB: database}},
		ScriptRunner:        runner,
	}
	err = resources.Shutdown(context.Background())
	if !errors.Is(err, ErrShutdownIncomplete) || !errors.Is(err, closeErr) {
		t.Fatalf("Shutdown error = %v, want incomplete tenant close", err)
	}
	if runner.closed.Load() != 0 {
		t.Fatal("script runner closed after tenant close failed")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessResourcesJoinsDirectoryMonitorBeforeClosingPlatformDB(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectClose()
	monitor, err := startDirectoryLagMonitor(context.Background(), func(ctx context.Context) { <-ctx.Done() }, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	resources := &ProcessResources{
		DirectoryLagMonitor: monitor,
		PlatformDB:          database,
	}
	if err := resources.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessResourcesDefersAndJoinsRegisteredWorkerStarts(t *testing.T) {
	resources := &ProcessResources{}
	var starts atomic.Int32
	if err := resources.RegisterWorkerStart(func() error { starts.Add(1); return nil }); err != nil {
		t.Fatal("RegisterWorkerStart:", err)
	}
	if starts.Load() != 0 {
		t.Fatal("assembly started a worker before Engine.Run")
	}
	if err := resources.StartRegisteredWorkers(); err != nil {
		t.Fatal("StartRegisteredWorkers:", err)
	}
	if starts.Load() != 1 {
		t.Fatalf("initial worker starts = %d, want 1", starts.Load())
	}
	if err := resources.StartRegisteredWorkers(); err != nil {
		t.Fatal("second StartRegisteredWorkers:", err)
	}
	if err := resources.RegisterWorkerStart(func() error { starts.Add(1); return nil }); err != nil {
		t.Fatal("live RegisterWorkerStart:", err)
	}
	if starts.Load() != 2 {
		t.Fatalf("live worker starts = %d, want 2", starts.Load())
	}
	if err := resources.Shutdown(context.Background()); err != nil {
		t.Fatal("Shutdown:", err)
	}
	if err := resources.RegisterWorkerStart(func() error { t.Error("started worker during shutdown"); return nil }); err == nil {
		t.Fatal("worker registration during shutdown succeeded")
	}
}

func TestProcessResourcesCancelsAndJoinsWorkerStartupBeforeClosingDependencies(t *testing.T) {
	canceled := make(chan struct{})
	entered := make(chan struct{})
	resources := &ProcessResources{CancelProcess: func() { close(canceled) }}
	if err := resources.RegisterWorkerStart(func() error {
		close(entered)
		<-canceled
		return nil
	}); err != nil {
		t.Fatal("RegisterWorkerStart:", err)
	}
	startDone := make(chan error, 1)
	go func() { startDone <- resources.StartRegisteredWorkers() }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker startup did not begin")
	}
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- resources.Shutdown(context.Background()) }()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatal("Shutdown:", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel and join registered worker startup")
	}
	if err := <-startDone; err != nil {
		t.Fatal("StartRegisteredWorkers:", err)
	}
}

func TestProcessResourcesStopsProducersBeforeDrainingAsyncHooks(t *testing.T) {
	done := make(chan struct{})
	var processCanceled atomic.Bool
	var workerCanceled atomic.Bool
	resources := &ProcessResources{
		StopAsyncHookWorker: func() {
			if !processCanceled.Load() {
				t.Error("async queue closed before process producers were canceled")
			}
			close(done)
		},
		AsyncHookWorkerDone: done,
		CancelAsyncHookWorker: func() {
			select {
			case <-done:
			default:
				t.Error("async hook worker canceled before its queue was drained")
			}
			workerCanceled.Store(true)
		},
		CancelProcess: func() {
			processCanceled.Store(true)
		},
	}
	if err := resources.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !processCanceled.Load() {
		t.Fatal("process cancellation did not run")
	}
}

func TestProcessResourcesAsyncHookTimeoutRetainsDependenciesAfterProducerCancellation(t *testing.T) {
	done := make(chan struct{})
	var processCanceled atomic.Bool
	var workerCanceled atomic.Bool
	runner := &testScriptRunner{}
	resources := &ProcessResources{
		StopAsyncHookWorker:   func() {},
		AsyncHookWorkerDone:   done,
		CancelAsyncHookWorker: func() { workerCanceled.Store(true) },
		CancelProcess:         func() { processCanceled.Store(true) },
		ScriptRunner:          runner,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := resources.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want deadline", err)
	}
	if !workerCanceled.Load() {
		t.Fatal("timed out async worker was not canceled")
	}
	if !processCanceled.Load() {
		t.Fatal("process producers were not canceled before async-worker timeout")
	}
	if got := runner.closed.Load(); got != 0 {
		t.Fatalf("script runner closed %d times while async worker may still use it", got)
	}
}

func TestProcessResourcesRetainsNATSWhenTenantDrainTimesOut(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectClose()
	runtime := &knet.LoadedSite{
		SiteID: "tenant-engine-drain-test",
		Name:   "drain.example",
		DB:     database,
		Config: knet.SiteRouterConfig{Hostname: "drain.example"},
	}
	router := knet.NewSiteRouter([]*knet.LoadedSite{runtime})
	handle, err := router.AcquireByID(context.Background(), runtime.SiteID)
	if err != nil {
		t.Fatal(err)
	}
	broker := &testNATSCloser{}
	resources := &ProcessResources{Tenants: router, NATSProvider: broker}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err = resources.Shutdown(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want tenant drain deadline", err)
	}
	if got := broker.closed.Load(); got != 0 {
		t.Fatalf("NATS closed %d times while tenant generation remained live", got)
	}
	handle.Release()
	if err := router.RuntimeManager().Close(context.Background()); err != nil {
		t.Fatalf("finish tenant shutdown after releasing handle: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessResourcesRetainsPlatformDependenciesOnOnboardingTimeout(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var processCanceled atomic.Bool
	var telemetryClosed atomic.Bool
	broker := &testNATSCloser{}
	runner := &testScriptRunner{}
	resources := &ProcessResources{
		CancelProcess: func() { processCanceled.Store(true) },
		CloseOnboardingJobs: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
		ScriptRunner: runner,
		NATSProvider: broker,
		PlatformDB:   database,
		TelemetryShutdown: func(context.Context) error {
			telemetryClosed.Store(true)
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err = resources.Shutdown(ctx)
	cancel()
	if !errors.Is(err, ErrShutdownIncomplete) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want incomplete onboarding timeout", err)
	}
	if !processCanceled.Load() {
		t.Fatal("process producers were not canceled")
	}
	if runner.closed.Load() != 0 || broker.closed.Load() != 0 || telemetryClosed.Load() {
		t.Fatal("shared providers closed while onboarding job may still use the platform database")
	}
	if err := database.Ping(); err != nil {
		t.Fatalf("platform database was closed while onboarding job may still use it: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("platform database must remain open while onboarding job may still use it: %v", err)
	}
}

func TestProcessResourcesClosesPlatformBeforeTelemetryLast(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectClose()
	var order []string
	broker := &testNATSCloser{onClose: func() { order = append(order, "nats") }}
	resources := &ProcessResources{
		NATSProvider: broker,
		PlatformDB:   database,
		TelemetryShutdown: func(context.Context) error {
			if err := database.Ping(); err == nil {
				t.Error("telemetry shut down before platform database closed")
			}
			order = append(order, "telemetry")
			return nil
		},
	}
	if err := resources.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if broker.closed.Load() != 1 {
		t.Fatalf("NATS close count = %d, want one", broker.closed.Load())
	}
	if len(order) != 2 || order[0] != "nats" || order[1] != "telemetry" {
		t.Fatalf("close order = %v, want [nats telemetry] with platform DB before telemetry", order)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessResourcesJoinsPlatformAndTelemetryCloseFailures(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	platformErr := errors.New("platform close failed")
	telemetryErr := errors.New("telemetry close failed")
	mock.ExpectClose().WillReturnError(platformErr)
	resources := &ProcessResources{
		PlatformDB: database,
		TelemetryShutdown: func(context.Context) error {
			return telemetryErr
		},
	}
	err = resources.Shutdown(context.Background())
	if !errors.Is(err, ErrShutdownIncomplete) || !errors.Is(err, platformErr) || !errors.Is(err, telemetryErr) {
		t.Fatalf("Shutdown error = %v, want incomplete marker and both close failures", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
