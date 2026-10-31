package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/asenawritescode/kora/contract"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/site"
)

type tenantDirectoryStub struct {
	mu sync.Mutex
	site.DirectoryChangeFeed
	descriptors     []contract.SiteDescriptor
	infos           []site.DBSiteInfo
	descriptor      contract.SiteDescriptor
	byID            site.DBSiteInfo
	byIDErr         error
	revision        uint64
	descriptorsByID map[string]contract.SiteDescriptor
	infosByID       map[string]site.DBSiteInfo
}

func (d *tenantDirectoryStub) GetDescriptorByID(siteID string) (site.SiteDescriptor, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if descriptor, ok := d.descriptorsByID[siteID]; ok {
		return descriptor, nil
	}
	return d.descriptor, nil
}
func (d *tenantDirectoryStub) GetByID(siteID string) (site.DBSiteInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if info, ok := d.infosByID[siteID]; ok {
		return info, nil
	}
	return d.byID, d.byIDErr
}
func (d *tenantDirectoryStub) GetDirectoryDescriptors() ([]contract.SiteDescriptor, error) {
	return append([]contract.SiteDescriptor(nil), d.descriptors...), nil
}
func (d *tenantDirectoryStub) GetDirectorySnapshotForCell(string) ([]site.DBSiteInfo, error) {
	return append([]site.DBSiteInfo(nil), d.infos...), nil
}
func (d *tenantDirectoryStub) GetByIDContext(ctx context.Context, siteID string) (site.DBSiteInfo, error) {
	if err := ctx.Err(); err != nil {
		return site.DBSiteInfo{}, err
	}
	return d.GetByID(siteID)
}
func (d *tenantDirectoryStub) GetDirectoryDescriptorPageContext(ctx context.Context, _ string, _ string, _ uint16) ([]site.SiteDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, nil
}
func (d *tenantDirectoryStub) CurrentDirectoryRevision() (uint64, error) { return d.revision, nil }

type tenantIngressStub struct {
	validated []contract.SiteDescriptor
	replaced  []contract.SiteDescriptor
	ready     map[string]bool
}

func (g *tenantIngressStub) ValidateDirectory(descriptors []contract.SiteDescriptor) error {
	g.validated = append([]contract.SiteDescriptor(nil), descriptors...)
	return nil
}
func (*tenantIngressStub) ApplyDescriptor(contract.SiteDescriptor) error { return nil }
func (g *tenantIngressStub) MarkLocalReady(siteID string, ready bool) {
	if g.ready == nil {
		g.ready = make(map[string]bool)
	}
	g.ready[siteID] = ready
}
func (g *tenantIngressStub) ReplaceDirectory(descriptors []contract.SiteDescriptor) error {
	g.replaced = append([]contract.SiteDescriptor(nil), descriptors...)
	return nil
}

func TestNewTenantDirectoryConsumerKeepsStableIdentityAndAdvancesDirectoryRevision(t *testing.T) {
	const tenantID = "tenant-directory-consumer"
	runtime := &knet.LoadedSite{
		SiteID: tenantID, Name: "directory.example", ConfigRevision: 5,
		DirectoryRevision: 2, Config: knet.SiteRouterConfig{Hostname: "directory.example"},
	}
	router := knet.NewSiteRouter([]*knet.LoadedSite{runtime})
	coordinator := testTenantCoordinator(t, router, nil)
	consumer, err := NewTenantDirectoryConsumer(TenantDirectoryConfig{
		Directory: &tenantDirectoryStub{}, Tenants: router, Coordinator: coordinator,
		ConsumerID: "engine:stable-replica", InitialCursor: 4,
	})
	if err != nil {
		t.Fatalf("NewTenantDirectoryConsumer: %v", err)
	}
	if consumer.ID != "engine:stable-replica" || consumer.InitialCursor != 4 || consumer.PageSize != 100 {
		t.Fatalf("consumer identity/cursor/page size = (%q, %d, %d)", consumer.ID, consumer.InitialCursor, consumer.PageSize)
	}
	if consumer.Apply == nil || consumer.Resync == nil {
		t.Fatal("directory consumer is missing apply or resync policy")
	}
	err = consumer.Apply(context.Background(), site.SiteDirectoryChange{
		Cursor:     7,
		Descriptor: contract.SiteDescriptor{SiteID: tenantID, Aliases: []string{"directory.example"}, Status: "active", ConfigRevision: 5},
	})
	if err != nil {
		t.Fatalf("apply unchanged config revision: %v", err)
	}
	active := router.SnapshotByID(tenantID)
	if active == nil || active.Activation.DirectoryRevision != 7 {
		t.Fatalf("directory revision after apply = %#v, want 7", active)
	}
}

func TestNewTenantDirectoryConsumerRequiresStableConsumerIdentity(t *testing.T) {
	_, err := NewTenantDirectoryConsumer(TenantDirectoryConfig{
		Directory: &tenantDirectoryStub{}, Tenants: knet.NewSiteRouter(nil),
	})
	if err == nil {
		t.Fatal("NewTenantDirectoryConsumer without identity succeeded")
	}
}

func TestTenantDirectoryResyncReplacesIngressAndRetiresMissingRuntime(t *testing.T) {
	const missingID = "tenant-resync-missing"
	runtime := &knet.LoadedSite{
		SiteID: missingID, Name: "old.example", Config: knet.SiteRouterConfig{Hostname: "old.example"},
	}
	router := knet.NewSiteRouter([]*knet.LoadedSite{runtime})
	directory := &tenantDirectoryStub{descriptors: []contract.SiteDescriptor{{
		SiteID: "tenant-remote", Status: "active", RuntimeCellID: "cell-b", ConfigRevision: 1,
	}}}
	ingress := &tenantIngressStub{}
	coordinator := testTenantCoordinator(t, router, ingress)
	consumer, err := NewTenantDirectoryConsumer(TenantDirectoryConfig{
		Directory: directory, Tenants: router, Coordinator: coordinator, CellID: "cell-a",
		ConsumerID: "engine:resync",
	})
	if err != nil {
		t.Fatalf("NewTenantDirectoryConsumer: %v", err)
	}
	if err := consumer.Resync(context.Background(), 8); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	if len(ingress.validated) != 1 || len(ingress.replaced) != 1 || ingress.ready["tenant-remote"] {
		t.Fatalf("ingress resync state = validated %v replaced %v ready %v", ingress.validated, ingress.replaced, ingress.ready)
	}
	if router.SnapshotByID(missingID) != nil || ingress.ready[missingID] {
		t.Fatalf("missing runtime was not retired from routing/ingress: snapshot=%v ready=%v", router.SnapshotByID(missingID), ingress.ready[missingID])
	}
	if err := coordinator.WaitRetired(context.Background()); err != nil {
		t.Fatalf("join retired tenant runtime: %v", err)
	}
}

func TestTenantDirectoryRejectsInvalidDeploymentAssignmentBeforePublishing(t *testing.T) {
	invalid := contract.SiteDescriptor{
		SiteID: "tenant-invalid-assignment", Aliases: []string{"invalid.example"}, Status: "active", RuntimeCellID: "cell/west",
	}
	directory := &tenantDirectoryStub{descriptors: []contract.SiteDescriptor{invalid}}
	router := knet.NewSiteRouter(nil)
	ingress := &tenantIngressStub{}
	coordinator := testTenantCoordinator(t, router, ingress)
	consumer, err := NewTenantDirectoryConsumer(TenantDirectoryConfig{
		Directory: directory, Tenants: router, Coordinator: coordinator, ConsumerID: "engine:assignment-validation",
	})
	if err != nil {
		t.Fatal("create directory consumer:", err)
	}
	if err := consumer.Resync(context.Background(), 9); err == nil {
		t.Fatal("resync accepted an invalid deployment assignment")
	}
	if len(ingress.replaced) != 0 || router.SnapshotByID(invalid.SiteID) != nil {
		t.Fatal("invalid resync published directory state")
	}
	if err := consumer.Apply(context.Background(), site.SiteDirectoryChange{Cursor: 10, Descriptor: invalid}); err == nil {
		t.Fatal("change feed accepted an invalid deployment assignment")
	}
	if len(ingress.replaced) != 0 || router.SnapshotByID(invalid.SiteID) != nil {
		t.Fatal("invalid change published directory state")
	}
}

func TestTenantDirectoryResyncDoesNotLoadCredentialsForColdActiveTenants(t *testing.T) {
	directory := &tenantDirectoryStub{
		descriptors: []contract.SiteDescriptor{{
			SiteID: "tenant-cold-resync", Aliases: []string{"cold-resync.example"}, Status: "active", ConfigRevision: 2,
		}},
		byIDErr: errors.New("tenant credentials must remain cold during directory resync"),
	}
	router := knet.NewSiteRouter(nil)
	coordinator := testTenantCoordinator(t, router, nil)
	consumer, err := NewTenantDirectoryConsumer(TenantDirectoryConfig{
		Directory: directory, Tenants: router, Coordinator: coordinator, ConsumerID: "engine:cold-resync",
	})
	if err != nil {
		t.Fatal("create directory consumer:", err)
	}
	if err := consumer.Resync(context.Background(), 4); err != nil {
		t.Fatalf("resync read tenant credentials for a cold runtime: %v", err)
	}
	if router.SnapshotByID("tenant-cold-resync") != nil {
		t.Fatal("directory resync activated a cold tenant")
	}
	if known, err := router.EnsureHost(context.Background(), "cold-resync.example", false); !known || err == nil {
		t.Fatalf("cold descriptor activation = (known=%t, err=%v), want known and fail-closed missing activation dependency", known, err)
	}
}

func testTenantCoordinator(t *testing.T, router *knet.SiteRouter, ingress TenantIngressDirectory) *TenantRuntimeCoordinator {
	t.Helper()
	coordinator, err := NewTenantRuntimeCoordinator(TenantRuntimeCoordinatorConfig{
		Tenants: router, Common: &site.CommonConfig{}, Installer: func(site.DBSiteInfo, *knet.LoadedSite) error { return nil },
		Ingress: ingress, RetainRuntime: func(*knet.LoadedSite) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func TestTenantDirectoryActivationUsesCoordinator(t *testing.T) {
	const tenantID = "tenant-directory-activation"
	info := site.DBSiteInfo{SiteID: tenantID, Name: "activate.example", Status: "active", ConfigRevision: 3}
	directory := &tenantDirectoryStub{
		descriptor: contract.SiteDescriptor{SiteID: tenantID, Aliases: []string{"activate.example"}, Status: "active", ConfigRevision: 3},
		byID:       info, revision: 9,
	}
	router := knet.NewSiteRouter(nil)
	installed := false
	coordinator, err := newTenantRuntimeCoordinator(TenantRuntimeCoordinatorConfig{
		Tenants: router, Directory: directory, Common: &site.CommonConfig{},
		Installer: func(got site.DBSiteInfo, _ *knet.LoadedSite) error {
			installed = got.SiteID == tenantID
			return nil
		}, RetainRuntime: func(*knet.LoadedSite) {},
	}, func(_ context.Context, got site.DBSiteInfo, _ *site.CommonConfig, _ *knet.LoadedSite, _ string) (*knet.LoadedSite, error) {
		return &knet.LoadedSite{SiteID: got.SiteID, Name: got.Name, ConfigRevision: got.ConfigRevision, Config: knet.SiteRouterConfig{Hostname: got.Name}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := NewTenantDirectoryConsumer(TenantDirectoryConfig{
		Directory: directory, Tenants: router, Coordinator: coordinator, ConsumerID: "engine:activation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.Apply(context.Background(), site.SiteDirectoryChange{
		Cursor: 9, Descriptor: directory.descriptor,
	}); err != nil {
		t.Fatalf("apply activation event: %v", err)
	}
	if router.SnapshotByID(tenantID) != nil || installed {
		t.Fatal("directory observation eagerly activated a cold tenant")
	}
	if known, err := router.EnsureHost(context.Background(), "activate.example", false); err != nil || !known {
		t.Fatalf("on-demand activation = (known=%t, err=%v)", known, err)
	}
	active := router.SnapshotByID(tenantID)
	if !installed || active == nil || active.Activation.DirectoryRevision != 9 {
		t.Fatalf("activation result installed=%t active=%#v", installed, active)
	}
}

func TestTenantDirectoryAssignmentEventRegistersColdTenantForLazyActivation(t *testing.T) {
	const tenantID = "tenant-deployment-assignment"
	descriptor := contract.SiteDescriptor{
		SiteID: tenantID, Aliases: []string{"assignment.example"}, Status: "active",
		ConfigRevision: 2, RuntimeCellID: "deployment-a",
	}
	directory := &tenantDirectoryStub{
		descriptor: descriptor,
		byID: site.DBSiteInfo{
			SiteID: tenantID, Name: "assignment.example", Status: "active", ConfigRevision: 2,
		}, revision: 2,
	}
	router := knet.NewSiteRouter(nil)
	ingress := &tenantIngressStub{}
	coordinator, err := newTenantRuntimeCoordinator(TenantRuntimeCoordinatorConfig{
		Tenants: router, Directory: directory, Common: &site.CommonConfig{}, Ingress: ingress,
		Installer:     func(site.DBSiteInfo, *knet.LoadedSite) error { return nil },
		RetainRuntime: func(*knet.LoadedSite) {},
	}, func(_ context.Context, info site.DBSiteInfo, _ *site.CommonConfig, _ *knet.LoadedSite, _ string) (*knet.LoadedSite, error) {
		return &knet.LoadedSite{
			SiteID: info.SiteID, Name: info.Name, ConfigRevision: info.ConfigRevision,
			Config: knet.SiteRouterConfig{Hostname: info.Name},
		}, nil
	})
	if err != nil {
		t.Fatal("create tenant runtime coordinator:", err)
	}
	consumer, err := NewTenantDirectoryConsumer(TenantDirectoryConfig{
		Directory: directory, Tenants: router, Coordinator: coordinator,
		CellID: "deployment-a", ConsumerID: "engine:deployment-a",
	})
	if err != nil {
		t.Fatal("create tenant directory consumer:", err)
	}
	if err := consumer.Apply(context.Background(), site.SiteDirectoryChange{
		Cursor: 2, Descriptor: descriptor,
	}); err != nil {
		t.Fatal("apply local assignment event:", err)
	}
	if router.SnapshotByID(tenantID) != nil {
		t.Fatal("directory event eagerly activated the tenant instead of preparing cold routing")
	}
	known, err := router.EnsureName(context.Background(), tenantID)
	if err != nil || !known {
		t.Fatalf("on-demand assigned-tenant activation = (known=%t, err=%v)", known, err)
	}
	if router.SnapshotByID(tenantID) == nil || !ingress.ready[tenantID] {
		t.Fatalf("assigned tenant not published locally: snapshot=%v ingress_ready=%t", router.SnapshotByID(tenantID), ingress.ready[tenantID])
	}
}

func TestConcurrentColdTenantRequestsShareOneActivation(t *testing.T) {
	const tenantID = "tenant-cold-activation"
	directory := &tenantDirectoryStub{
		descriptor: contract.SiteDescriptor{SiteID: tenantID, Aliases: []string{"cold.example"}, Status: "active", ConfigRevision: 3},
		byID:       site.DBSiteInfo{SiteID: tenantID, Name: "cold.example", Status: "active", ConfigRevision: 3}, revision: 11,
	}
	router := knet.NewSiteRouter(nil)
	var loads atomic.Int32
	var installs atomic.Int32
	coordinator, err := newTenantRuntimeCoordinator(TenantRuntimeCoordinatorConfig{
		Tenants: router, Directory: directory, Common: &site.CommonConfig{},
		Installer:     func(site.DBSiteInfo, *knet.LoadedSite) error { installs.Add(1); return nil },
		RetainRuntime: func(*knet.LoadedSite) {},
	}, func(_ context.Context, info site.DBSiteInfo, _ *site.CommonConfig, _ *knet.LoadedSite, _ string) (*knet.LoadedSite, error) {
		loads.Add(1)
		return &knet.LoadedSite{
			SiteID: info.SiteID, Name: info.Name, ConfigRevision: info.ConfigRevision,
			Config: knet.SiteRouterConfig{Hostname: info.Name, Domains: append([]string(nil), info.Domains...)},
		}, nil
	})
	if err != nil {
		t.Fatal("create coordinator:", err)
	}
	router.SetLazyDirectory([]contract.SiteDescriptor{directory.descriptor}, coordinator.ActivateFromDirectory)
	const requests = 24
	var wg sync.WaitGroup
	errs := make(chan error, requests)
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			known, err := router.EnsureHost(context.Background(), "cold.example", false)
			if err != nil {
				errs <- err
			} else if !known {
				errs <- errors.New("cold tenant descriptor was not found")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error("cold activation:", err)
	}
	if loads.Load() != 1 || installs.Load() != 1 {
		t.Fatalf("cold activation loads/installs = %d/%d, want 1/1", loads.Load(), installs.Load())
	}
	snapshot := router.SnapshotByID(tenantID)
	if snapshot == nil || snapshot.Activation.DirectoryRevision != 11 {
		t.Fatalf("activated snapshot = %#v, want directory revision 11", snapshot)
	}
}

func TestResidentTenantLimitEvictsLeastRecentlyUsedIdleTenantForColdActivation(t *testing.T) {
	const limit = uint32(2)
	router := knet.NewSiteRouter(nil)
	directory := &tenantDirectoryStub{revision: 5}
	var loads atomic.Int32
	coordinator, err := newTenantRuntimeCoordinator(TenantRuntimeCoordinatorConfig{
		Tenants: router, Directory: directory, Common: &site.CommonConfig{},
		MaxResidentTenants: limit,
		Installer:          func(site.DBSiteInfo, *knet.LoadedSite) error { return nil }, RetainRuntime: func(*knet.LoadedSite) {},
	}, func(_ context.Context, info site.DBSiteInfo, _ *site.CommonConfig, _ *knet.LoadedSite, _ string) (*knet.LoadedSite, error) {
		loads.Add(1)
		return &knet.LoadedSite{SiteID: info.SiteID, Name: info.Name, ConfigRevision: info.ConfigRevision, Config: knet.SiteRouterConfig{Hostname: info.Name}}, nil
	})
	if err != nil {
		t.Fatal("create coordinator:", err)
	}
	for index, id := range []string{"tenant-one", "tenant-two", "tenant-three"} {
		name := id + ".example"
		directory.descriptor = contract.SiteDescriptor{SiteID: id, Aliases: []string{name}, Status: "active", ConfigRevision: 1}
		directory.byID = site.DBSiteInfo{SiteID: id, Name: name, Status: "active", ConfigRevision: 1}
		err := coordinator.ActivateFromDirectory(context.Background(), id)
		if index < 2 && err != nil {
			t.Fatalf("activation %s: %v", id, err)
		}
		if index == 2 && err != nil {
			t.Fatalf("third activation after safe idle eviction: %v", err)
		}
	}
	if got, _ := router.ResourceCounts(); got != int(limit) {
		t.Fatalf("resident count = %d, want %d", got, limit)
	}
	if got := loads.Load(); got != int32(limit+1) {
		t.Fatalf("runtime loads = %d, want initial tenants plus cold activation (%d)", got, limit+1)
	}
	if router.SnapshotByID("tenant-one") != nil || router.SnapshotByID("tenant-three") == nil {
		t.Fatal("cold activation did not evict the least recently used idle runtime")
	}
}

func TestResidentLimitSerializesDistinctActivationsAndKeepsCapacityBound(t *testing.T) {
	router := knet.NewSiteRouter(nil)
	directory := &tenantDirectoryStub{revision: 1}
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	started := make(chan struct{}, 3)
	coordinator, err := newTenantRuntimeCoordinator(TenantRuntimeCoordinatorConfig{
		Tenants: router, Directory: directory, Common: &site.CommonConfig{},
		MaxResidentTenants: 2,
		Installer:          func(site.DBSiteInfo, *knet.LoadedSite) error { return nil }, RetainRuntime: func(*knet.LoadedSite) {},
	}, func(_ context.Context, info site.DBSiteInfo, _ *site.CommonConfig, _ *knet.LoadedSite, _ string) (*knet.LoadedSite, error) {
		current := inFlight.Add(1)
		for observed := maxInFlight.Load(); current > observed && !maxInFlight.CompareAndSwap(observed, current); observed = maxInFlight.Load() {
		}
		started <- struct{}{}
		inFlight.Add(-1)
		return &knet.LoadedSite{SiteID: info.SiteID, Name: info.Name, ConfigRevision: 1, Config: knet.SiteRouterConfig{Hostname: info.Name}}, nil
	})
	if err != nil {
		t.Fatal("create coordinator:", err)
	}
	errs := make(chan error, 3)
	for _, id := range []string{"concurrent-a", "concurrent-b", "concurrent-c"} {
		id := id
		directory.mu.Lock()
		if directory.descriptorsByID == nil {
			directory.descriptorsByID = make(map[string]contract.SiteDescriptor)
		}
		if directory.infosByID == nil {
			directory.infosByID = make(map[string]site.DBSiteInfo)
		}
		directory.descriptorsByID[id] = contract.SiteDescriptor{SiteID: id, Aliases: []string{id + ".example"}, Status: "active", ConfigRevision: 1}
		directory.infosByID[id] = site.DBSiteInfo{SiteID: id, Name: id + ".example", Status: "active", ConfigRevision: 1}
		directory.mu.Unlock()
		go func() {
			errs <- coordinator.ActivateFromDirectory(context.Background(), id)
		}()
	}
	for range 3 {
		if err := <-errs; err != nil {
			t.Errorf("activation error = %v", err)
		}
	}
	if got := len(started); got != 3 {
		t.Fatalf("tenant loads admitted = %d, want each request after safe eviction", got)
	}
	if got := maxInFlight.Load(); got != 1 {
		t.Fatalf("maximum concurrent tenant loads = %d, want serialized coordinator admission", got)
	}
	if active, retiring := router.ResourceCounts(); active != 2 || retiring != 0 {
		t.Fatalf("active/retiring resident counts = %d/%d, want 2/0", active, retiring)
	}
}
