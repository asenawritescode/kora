package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/tenantruntime"
)

func TestTenantCoordinatorBoundsActivationWaiters(t *testing.T) {
	entered, release := make(chan struct{}, 2), make(chan struct{})
	coordinator, err := newTenantRuntimeCoordinator(TenantRuntimeCoordinatorConfig{
		Tenants: knet.NewSiteRouter(nil), Common: &site.CommonConfig{}, MaxActivationWaiters: 1,
		Installer: func(site.DBSiteInfo, *knet.LoadedSite) error { return nil }, RetainRuntime: func(*knet.LoadedSite) {},
	}, func(_ context.Context, info site.DBSiteInfo, _ *site.CommonConfig, _ *knet.LoadedSite, _ string) (*knet.LoadedSite, error) {
		entered <- struct{}{}
		<-release
		return &knet.LoadedSite{SiteID: info.SiteID, Name: info.Name}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() {
		first <- coordinator.Add(context.Background(), site.DBSiteInfo{SiteID: "first", Name: "first.example"}, 1)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first activation did not start")
	}
	second := make(chan error, 1)
	go func() {
		second <- coordinator.Add(context.Background(), site.DBSiteInfo{SiteID: "second", Name: "second.example"}, 1)
	}()
	deadline := time.Now().Add(time.Second)
	for coordinator.AdmissionSnapshot().Waiters != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	thirdErr := coordinator.Add(context.Background(), site.DBSiteInfo{SiteID: "third", Name: "third.example"}, 1)
	if !errors.Is(thirdErr, tenantruntime.ErrActivationCapacity) {
		t.Fatalf("third activation error = %v, want bounded admission error", thirdErr)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal("first activation:", err)
	}
	if err := <-second; err != nil {
		t.Fatal("second activation:", err)
	}
	if snapshot := coordinator.AdmissionSnapshot(); snapshot.Waiters != 0 || snapshot.Rejected != 1 {
		t.Fatalf("admission snapshot = %+v", snapshot)
	}
}

func TestManagementRetireWaitsForAdmittedTenantRequests(t *testing.T) {
	const tenantID = "management-drain-test"
	runtime := &knet.LoadedSite{SiteID: tenantID, Name: "drain.example"}
	router := knet.NewSiteRouter([]*knet.LoadedSite{runtime})
	_, handle, err := router.PinExistingByID(context.Background(), tenantID)
	if err != nil {
		t.Fatal("admit tenant request:", err)
	}
	coordinator, err := newTenantRuntimeCoordinator(TenantRuntimeCoordinatorConfig{
		Tenants: router, Common: &site.CommonConfig{}, DrainTimeout: time.Second,
		Installer:     func(site.DBSiteInfo, *knet.LoadedSite) error { return nil },
		RetainRuntime: func(*knet.LoadedSite) {},
	}, func(context.Context, site.DBSiteInfo, *site.CommonConfig, *knet.LoadedSite, string) (*knet.LoadedSite, error) {
		return nil, errors.New("loader should not be used")
	})
	if err != nil {
		t.Fatal("create coordinator:", err)
	}
	retired := make(chan error, 1)
	go func() { retired <- coordinator.Retire(context.Background(), tenantID) }()
	deadline := time.Now().Add(time.Second)
	for router.SnapshotByID(tenantID) != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if router.SnapshotByID(tenantID) != nil {
		t.Fatal("retired tenant remained routable")
	}
	select {
	case err := <-retired:
		t.Fatalf("management retirement completed before admitted request drained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	handle.Release()
	select {
	case err := <-retired:
		if err != nil {
			t.Fatal("retire after request drain:", err)
		}
	case <-time.After(time.Second):
		t.Fatal("management retirement did not join the admitted request")
	}
	if _, retiring := router.ResourceCounts(); retiring != 0 {
		t.Fatalf("retired tenant still consumes retirement capacity: %d", retiring)
	}
}

func TestManagementRetireRetryJoinsDrainAfterRequestDeadline(t *testing.T) {
	const tenantID = "management-drain-recovery-test"
	router := knet.NewSiteRouter([]*knet.LoadedSite{{SiteID: tenantID, Name: "drain-recovery.example"}})
	_, handle, err := router.PinExistingByID(context.Background(), tenantID)
	if err != nil {
		t.Fatal("admit tenant request:", err)
	}
	coordinator, err := newTenantRuntimeCoordinator(TenantRuntimeCoordinatorConfig{
		Tenants: router, Common: &site.CommonConfig{}, DrainTimeout: time.Second,
		Installer:     func(site.DBSiteInfo, *knet.LoadedSite) error { return nil },
		RetainRuntime: func(*knet.LoadedSite) {},
	}, func(context.Context, site.DBSiteInfo, *site.CommonConfig, *knet.LoadedSite, string) (*knet.LoadedSite, error) {
		return nil, errors.New("loader should not be used")
	})
	if err != nil {
		t.Fatal("create coordinator:", err)
	}
	firstCtx, cancelFirst := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelFirst()
	if err := coordinator.Retire(firstCtx, tenantID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first management retirement error = %v, want deadline", err)
	}
	if router.SnapshotByID(tenantID) != nil {
		t.Fatal("timed-out retirement left tenant routable")
	}
	retry := make(chan error, 1)
	go func() { retry <- coordinator.Retire(context.Background(), tenantID) }()
	select {
	case err := <-retry:
		t.Fatalf("management retry returned before the original drain completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	handle.Release()
	select {
	case err := <-retry:
		if err != nil {
			t.Fatal("management retirement retry:", err)
		}
	case <-time.After(time.Second):
		t.Fatal("management retry did not join the original drain")
	}
}

func TestTenantActivationWorkAdmissionRejectsBeforeRuntimeLoad(t *testing.T) {
	loadCalls := 0
	admissionCalls := 0
	coordinator, err := newTenantRuntimeCoordinator(TenantRuntimeCoordinatorConfig{
		Tenants: knet.NewSiteRouter(nil), Common: &site.CommonConfig{},
		ActivationWorkAdmission: func(context.Context, site.DBSiteInfo) (func(), error) {
			admissionCalls++
			return nil, errors.New("tenant DB work capacity exhausted")
		},
		Installer:     func(site.DBSiteInfo, *knet.LoadedSite) error { return nil },
		RetainRuntime: func(*knet.LoadedSite) {},
	}, func(_ context.Context, _ site.DBSiteInfo, _ *site.CommonConfig, _ *knet.LoadedSite, _ string) (*knet.LoadedSite, error) {
		loadCalls++
		return &knet.LoadedSite{SiteID: "activation-test", Name: "activation.example"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = coordinator.Add(context.Background(), site.DBSiteInfo{SiteID: "activation-test", Name: "activation.example"}, 1)
	if err == nil || !strings.Contains(err.Error(), "capacity exhausted") {
		t.Fatalf("activation error = %v, want work-capacity rejection", err)
	}
	if admissionCalls != 1 || loadCalls != 0 {
		t.Fatalf("activation admission/load calls = %d/%d, want 1/0", admissionCalls, loadCalls)
	}
}

func TestTenantCoordinatorWaitHonorsContextAndDeadline(t *testing.T) {
	coordinator, err := newTenantRuntimeCoordinator(TenantRuntimeCoordinatorConfig{
		Tenants: knet.NewSiteRouter(nil), Common: &site.CommonConfig{}, MaxActivationWaiters: 1, ActivationWait: time.Second,
		Installer: func(site.DBSiteInfo, *knet.LoadedSite) error { return nil }, RetainRuntime: func(*knet.LoadedSite) {},
	}, func(_ context.Context, info site.DBSiteInfo, _ *site.CommonConfig, _ *knet.LoadedSite, _ string) (*knet.LoadedSite, error) {
		return &knet.LoadedSite{SiteID: info.SiteID, Name: info.Name}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	release, err := coordinator.acquireOperation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan error, 1)
	go func() { _, err := coordinator.acquireOperation(ctx); waiting <- err }()
	deadline := time.Now().Add(time.Second)
	for coordinator.AdmissionSnapshot().Waiters != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-waiting; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v", err)
	}
	if got := coordinator.AdmissionSnapshot().Waiters; got != 0 {
		t.Fatalf("waiter count after cancellation = %d", got)
	}
	release()

	release, err = coordinator.acquireOperation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	coordinator.operationWait = 10 * time.Millisecond
	if _, err := coordinator.acquireOperation(context.Background()); !errors.Is(err, tenantruntime.ErrActivationCapacity) {
		t.Fatalf("expired waiter error = %v", err)
	}
	release()
}

func TestTenantCoordinatorFailedCompatibleReplacementKeepsOldRuntimeLive(t *testing.T) {
	database, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectPing()
	const tenantID = "tenant-coordinator-replace"
	old := &knet.LoadedSite{SiteID: tenantID, Name: "coordinator.example", DB: database, Config: knet.SiteRouterConfig{Hostname: "coordinator.example"}}
	router := knet.NewSiteRouter([]*knet.LoadedSite{old})
	installErr := errors.New("candidate service setup failed")
	coordinator, err := newTenantRuntimeCoordinator(TenantRuntimeCoordinatorConfig{
		Tenants: router, Common: &site.CommonConfig{},
		Installer:     func(site.DBSiteInfo, *knet.LoadedSite) error { return installErr },
		RetainRuntime: func(*knet.LoadedSite) {},
	}, func(_ context.Context, info site.DBSiteInfo, _ *site.CommonConfig, previous *knet.LoadedSite, _ string) (*knet.LoadedSite, error) {
		return &knet.LoadedSite{SiteID: info.SiteID, Name: info.Name, DB: previous.DB, Config: previous.Config}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = coordinator.Replace(context.Background(), site.DBSiteInfo{SiteID: tenantID, Name: old.Name}, 12)
	if !errors.Is(err, installErr) {
		t.Fatalf("Replace error = %v, want installer failure", err)
	}
	if active := router.SnapshotByID(tenantID); active == nil || active.Generation == 0 {
		t.Fatalf("old runtime was removed after failed replacement: %#v", active)
	}
	if err := database.Ping(); err != nil {
		t.Fatalf("old generation's shared database was closed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
