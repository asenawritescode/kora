package tenantruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
)

func runtimeFixture(id, name string, rev, directory uint64) *Runtime {
	return &Runtime{SiteID: id, Name: name, ConfigRevision: rev, DirectoryRevision: directory, DatabaseIdentity: "resource-a", Config: SiteConfig{Hostname: name, Domains: []string{name}}}
}

func TestManagerEnforcesConfiguredResidentLimit(t *testing.T) {
	manager, err := NewManager([]*Runtime{runtimeFixture("resident-1", "one.example", 1, 1)}, 2, 2)
	if err != nil {
		t.Fatal("create capped manager:", err)
	}
	if err := manager.Add(context.Background(), runtimeFixture("resident-2", "two.example", 1, 1)); err != nil {
		t.Fatal("add within capacity:", err)
	}
	if err := manager.Add(context.Background(), runtimeFixture("resident-3", "three.example", 1, 1)); !errors.Is(err, ErrResidentCapacity) {
		t.Fatalf("add over capacity error = %v, want ErrResidentCapacity", err)
	}
	if active, _ := manager.ResourceCounts(); active != 2 {
		t.Fatalf("active resident count = %d, want 2", active)
	}
	if _, err := NewManager([]*Runtime{runtimeFixture("resident-a", "a.example", 1, 1), runtimeFixture("resident-b", "b.example", 1, 1)}, 2, 1); !errors.Is(err, ErrResidentCapacity) {
		t.Fatalf("over-cap initial manager error = %v, want ErrResidentCapacity", err)
	}
}

func TestManagerEvictsLeastRecentlyUsedIdleRuntimeAndProtectsBusyRuntime(t *testing.T) {
	old := runtimeFixture("evict-old", "old.example", 1, 1)
	newer := runtimeFixture("evict-new", "new.example", 1, 1)
	old.lastUsedAt.Store(time.Now().Add(-time.Minute).UnixNano())
	newer.lastUsedAt.Store(time.Now().UnixNano())
	manager, err := NewManager([]*Runtime{old, newer}, 1, 2)
	if err != nil {
		t.Fatal("create manager:", err)
	}
	busyHandle, err := manager.Acquire(context.Background(), "evict-new")
	if err != nil {
		t.Fatal("pin newer runtime:", err)
	}
	retired, err := manager.EvictLeastRecentlyUsedIdle(context.Background())
	if err != nil {
		t.Fatal("evict idle least-recently-used runtime:", err)
	}
	if retired != old || manager.SnapshotByID("evict-old") != nil || manager.SnapshotByID("evict-new") == nil {
		t.Fatal("eviction did not retire only the older idle runtime")
	}
	if active, retiring := manager.ResourceCounts(); active != 1 || retiring != 1 {
		t.Fatalf("active/retiring count = %d/%d, want 1/1 until close", active, retiring)
	}
	manager.FinishRetirement(retired)
	busyHandle.Release()
	if got, err := manager.EvictLeastRecentlyUsedIdle(context.Background()); err != nil || got != newer {
		t.Fatalf("evict remaining idle runtime = (%v, %v), want newer", got, err)
	}
}

func TestManagerWillNotEvictRuntimeWithAdmittedHandle(t *testing.T) {
	runtime := runtimeFixture("busy", "busy.example", 1, 1)
	manager, err := NewManager([]*Runtime{runtime}, 1, 1)
	if err != nil {
		t.Fatal("create manager:", err)
	}
	handle, err := manager.Acquire(context.Background(), "busy")
	if err != nil {
		t.Fatal("acquire busy runtime:", err)
	}
	defer handle.Release()
	if retired, err := manager.EvictLeastRecentlyUsedIdle(context.Background()); !errors.Is(err, ErrNoIdleTenantRuntime) || retired != nil {
		t.Fatalf("evict busy runtime = (%v, %v), want ErrNoIdleTenantRuntime", retired, err)
	}
	if manager.SnapshotByID("busy") == nil {
		t.Fatal("busy runtime was removed from active routing")
	}
}

func TestManagerAcquirePinsSnapshotAndSameRevisionReplacement(t *testing.T) {
	old := runtimeFixture("tenant-1", "tenant.example", 4, 8)
	manager, err := NewManager([]*Runtime{old}, 2)
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.Acquire(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	oldGeneration := first.Generation()
	next := runtimeFixture("tenant-1", "tenant.example", 4, 9)
	next.DatabaseIdentity = old.DatabaseIdentity
	retired, err := manager.Activate(context.Background(), activationFor(next), next)
	if err != nil {
		t.Fatal(err)
	}
	if retired != old {
		t.Fatal("activation did not return the previous runtime")
	}
	if first.Snapshot().Generation != oldGeneration {
		t.Fatal("existing request handle changed generations")
	}
	second, err := manager.Acquire(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation() == oldGeneration || second.Snapshot().Activation.DirectoryRevision != 9 {
		t.Fatal("same-revision replacement was not published as a new generation")
	}
	first.Release()
	second.Release()
	manager.FinishRetirement(old)
}

func TestManagerCloseJoinsAddRetirementBeforeClosingTenantDatabase(t *testing.T) {
	oldDB, oldMock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create old tenant database:", err)
	}
	newDB, newMock, err := sqlmock.New()
	if err != nil {
		_ = oldDB.Close()
		t.Fatal("create replacement tenant database:", err)
	}
	oldMock.ExpectClose()
	newMock.ExpectClose()
	old := runtimeFixture("tenant-1", "tenant.example", 1, 1)
	old.DatabaseIdentity = "old-resource"
	old.DB = oldDB
	worker := &blockedRuntimeWorker{entered: make(chan struct{}), release: make(chan struct{})}
	old.AnalyticsWorker = worker
	manager, err := NewManager([]*Runtime{old}, 1)
	if err != nil {
		t.Fatal("create tenant manager:", err)
	}
	replacement := runtimeFixture("tenant-1", "tenant.example", 2, 2)
	replacement.DatabaseIdentity = "new-resource"
	replacement.DB = newDB
	if err := manager.Add(context.Background(), replacement); err != nil {
		t.Fatal("add replacement tenant runtime:", err)
	}
	select {
	case <-worker.entered:
	case <-time.After(time.Second):
		t.Fatal("manager did not start retiring the prior tenant generation")
	}
	if err := oldDB.Ping(); err != nil {
		t.Fatalf("old tenant DB closed while its analytics worker was still running: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err = manager.Close(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("manager close error = %v, want retirement join deadline", err)
	}
	if err := oldDB.Ping(); err != nil {
		t.Fatalf("old tenant DB closed after timed-out retirement join: %v", err)
	}
	close(worker.release)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatalf("retry manager close after worker exit: %v", err)
	}
	if err := oldMock.ExpectationsWereMet(); err != nil {
		t.Fatal("old tenant close:", err)
	}
	if err := newMock.ExpectationsWereMet(); err != nil {
		t.Fatal("replacement tenant close:", err)
	}
}

type blockedRuntimeWorker struct {
	entered chan struct{}
	release chan struct{}
}

func (*blockedRuntimeWorker) StartAsyncContext(context.Context) {}
func (w *blockedRuntimeWorker) Stop() {
	close(w.entered)
	<-w.release
}
func (*blockedRuntimeWorker) InvalidateMetrics(string) {}
func (*blockedRuntimeWorker) InvalidateAllMetrics()    {}
func (*blockedRuntimeWorker) Flush() error             { return nil }

func TestManagerFailedActivationPreservesCurrentSnapshot(t *testing.T) {
	current := runtimeFixture("tenant-1", "tenant.example", 5, 10)
	other := runtimeFixture("tenant-2", "other.example", 2, 4)
	manager, err := NewManager([]*Runtime{current, other}, 2)
	if err != nil {
		t.Fatal(err)
	}
	candidate := runtimeFixture("tenant-1", "other.example", 6, 11)
	if _, err := manager.Activate(context.Background(), activationFor(candidate), candidate); err == nil {
		t.Fatal("alias collision activation succeeded")
	}
	handle, err := manager.Acquire(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()
	if handle.Snapshot().Generation != current.EnsureRuntimeGeneration() {
		t.Fatal("failed activation replaced the current snapshot")
	}
}

func TestManagerRejectsMalformedRevisionAndIdentityChanges(t *testing.T) {
	current := runtimeFixture("tenant-1", "tenant.example", 5, 10)
	manager, err := NewManager([]*Runtime{current}, 2)
	if err != nil {
		t.Fatal(err)
	}
	malformed := runtimeFixture("tenant-1", "tenant.example", 0, 11)
	activation := activationFor(malformed)
	activation.ConfigRevision = "not-a-number"
	if _, err := manager.Activate(context.Background(), activation, malformed); !errors.Is(err, ErrActivationMismatch) {
		t.Fatalf("malformed revision error=%v, want ErrActivationMismatch", err)
	}
	changedID := runtimeFixture("tenant-2", "tenant.example", 6, 11)
	if _, err := manager.Activate(context.Background(), activationFor(changedID), changedID); err == nil {
		t.Fatal("replacement changed stable tenant ID without error")
	}
	handle, err := manager.Acquire(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()
	if handle.Snapshot().Generation != current.EnsureRuntimeGeneration() {
		t.Fatal("rejected activation changed the active runtime")
	}
}

func TestManagerIgnoresEmptyTenantIDsWhenCheckingDuplicates(t *testing.T) {
	first := runtimeFixture("", "one.example", 1, 1)
	second := runtimeFixture("", "two.example", 1, 1)
	manager, err := NewManager([]*Runtime{first}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(context.Background(), activationFor(second), second); err != nil {
		t.Fatalf("activate ID-less runtime: %v", err)
	}
	if got := manager.All(); len(got) != 2 {
		t.Fatalf("runtime count=%d, want 2", len(got))
	}
}

func TestManagerMissingRetireDoesNotWaitForCapacity(t *testing.T) {
	first := runtimeFixture("tenant-1", "tenant.example", 1, 1)
	manager, err := NewManager([]*Runtime{first}, 1)
	if err != nil {
		t.Fatal(err)
	}
	replacement := runtimeFixture("tenant-1", "tenant.example", 1, 2)
	if _, err := manager.Activate(context.Background(), activationFor(replacement), replacement); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if retired, err := manager.Retire(ctx, contract.TenantID("missing")); err != nil || retired != nil {
		t.Fatalf("Retire missing tenant=(%v, %v), want (nil, nil)", retired, err)
	}
}

func TestManagerBoundsRetirementsBeforeReplacing(t *testing.T) {
	first := runtimeFixture("tenant-1", "tenant.example", 1, 1)
	manager, err := NewManager([]*Runtime{first}, 1)
	if err != nil {
		t.Fatal(err)
	}
	second := runtimeFixture("tenant-1", "tenant.example", 1, 2)
	if _, err := manager.Activate(context.Background(), activationFor(second), second); err != nil {
		t.Fatal(err)
	}
	third := runtimeFixture("tenant-1", "tenant.example", 2, 3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.Activate(ctx, activationFor(third), third); !errors.Is(err, context.Canceled) {
		t.Fatalf("replacement under exhausted capacity error=%v, want canceled", err)
	}
	active, err := manager.Acquire(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if active.Snapshot().Activation.DirectoryRevision != 2 {
		t.Fatal("failed capacity admission changed the active snapshot")
	}
	active.Release()
	manager.FinishRetirement(first)
	if _, err := manager.Activate(context.Background(), activationFor(third), third); err != nil {
		t.Fatalf("replacement after drain: %v", err)
	}
}

func TestManagerCloseDrainsAndRejectsAdmission(t *testing.T) {
	runtime := runtimeFixture("tenant-1", "tenant.example", 1, 1)
	manager, err := NewManager([]*Runtime{runtime}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire(context.Background(), "tenant-1"); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("Acquire after close error=%v", err)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("idempotent Close: %v", err)
	}
}

func TestManagerCloseTimeoutRetainsPoolUntilHandleRelease(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	runtime := runtimeFixture("tenant-1", "tenant.example", 1, 1)
	runtime.DB = database
	mock.ExpectClose()
	manager, err := NewManager([]*Runtime{runtime}, 1)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err = manager.Close(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error=%v, want deadline", err)
	}
	if err := database.Ping(); err != nil {
		t.Fatalf("manager closed DB while handle was live: %v", err)
	}
	handle.Release()
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("retry Close after release: %v", err)
	}
	if err := database.Ping(); err == nil {
		t.Fatal("manager left tenant DB open after successful close")
	}
}

func TestManagerCloseTimeoutRetainsPoolUntilWorkerJoin(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	runtime := runtimeFixture("tenant-worker", "worker.example", 1, 1)
	runtime.DB = database
	manager, err := NewManager([]*Runtime{runtime}, 1)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	if !runtime.StartWorker(context.Background(), func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		<-release
	}) {
		t.Fatal("tenant worker did not start")
	}
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err = manager.Close(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error=%v, want worker join deadline", err)
	}
	if err := database.Ping(); err != nil {
		t.Fatalf("manager closed DB while worker remained live: %v", err)
	}
	close(release)
	mock.ExpectClose()
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("retry Close after worker exit: %v", err)
	}
	if err := database.Ping(); err == nil {
		t.Fatal("manager left tenant DB open after worker joined")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerUnavailableTenantStopsAdmissionAndCanDrain(t *testing.T) {
	runtime := runtimeFixture("tenant-1", "tenant.example", 1, 1)
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime.DB = database
	mock.ExpectClose()
	manager, err := NewManager([]*Runtime{runtime}, 1)
	if err != nil {
		t.Fatal(err)
	}
	inFlight, err := manager.Acquire(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetStatus("tenant-1", "deleting", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire(context.Background(), "tenant-1"); !errors.Is(err, ErrTenantNotActive) {
		t.Fatalf("Acquire deleting tenant error=%v", err)
	}
	idleCtx, cancelIdle := context.WithTimeout(context.Background(), 10*time.Millisecond)
	if err := manager.WaitForIdle(idleCtx, "tenant-1"); !errors.Is(err, context.DeadlineExceeded) {
		cancelIdle()
		t.Fatalf("WaitForIdle with active request error=%v", err)
	}
	cancelIdle()
	if err := database.Ping(); err != nil {
		t.Fatalf("DB closed before in-flight request finished: %v", err)
	}
	inFlight.Release()
	if err := manager.WaitForIdle(context.Background(), "tenant-1"); err != nil {
		t.Fatal(err)
	}
	retired, err := manager.RetireUnavailable(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if retired != runtime {
		t.Fatal("retired runtime mismatch")
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.FinishRetirement(runtime)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerCanPinUnavailableGenerationForResourceHandoff(t *testing.T) {
	runtime := runtimeFixture("tenant-handoff", "handoff.example", 1, 1)
	manager, err := NewManager([]*Runtime{runtime}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetStatus(contract.TenantID(runtime.SiteID), "deleting", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire(context.Background(), contract.TenantID(runtime.SiteID)); !errors.Is(err, ErrTenantNotActive) {
		t.Fatalf("request acquire during deletion error=%v, want ErrTenantNotActive", err)
	}
	got, handle, err := manager.PinExisting(context.Background(), contract.TenantID(runtime.SiteID))
	if err != nil {
		t.Fatalf("pin unavailable runtime for handoff: %v", err)
	}
	if got != runtime || handle.Snapshot().Status != "deleting" {
		t.Fatal("handoff pin did not return the fenced generation")
	}
	handle.Release()
}

func TestManagerReturnsCopiedSnapshotsForMetadataReaders(t *testing.T) {
	runtime := runtimeFixture("tenant-snapshot", "snapshot.example", 1, 1)
	manager, err := NewManager([]*Runtime{runtime}, 1)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := manager.AllSnapshots()
	if len(snapshots) != 1 {
		t.Fatalf("snapshot count=%d, want 1", len(snapshots))
	}
	snapshots[0].Config.Domains[0] = "changed.example"
	byID := manager.SnapshotByID(contract.TenantID(runtime.SiteID))
	if byID == nil || byID.Config.Domains[0] != "snapshot.example" {
		t.Fatalf("metadata snapshot mutation escaped manager: %#v", byID)
	}
	byName := manager.SnapshotByName(runtime.Name)
	if byName == nil || byName.Activation.Tenant != contract.TenantID(runtime.SiteID) {
		t.Fatalf("name snapshot resolved wrong tenant: %#v", byName)
	}
}
