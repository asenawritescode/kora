package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/analytics"
	"github.com/asenawritescode/kora/internal/db"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
)

type assemblyTestDirectory struct {
	*tenantDirectoryStub
	revision uint64
}

func (d *assemblyTestDirectory) CurrentDirectoryRevision() (uint64, error) {
	return d.revision, nil
}

func TestAssembleTenantRuntimeLeavesFailedActivationWithRollbackOwner(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create tenant DB mock:", err)
	}
	mock.ExpectClose()
	activationErr := errors.New("tenant directory read failed")
	runtime := &knet.LoadedSite{
		SiteID: "tenant-assembly", Name: "assembly.example", DB: database,
		Config: knet.SiteRouterConfig{Hostname: "assembly.example"},
	}
	resources := &ProcessResources{UnregisteredTenants: []*knet.LoadedSite{runtime}}
	directory := &assemblyTestDirectory{tenantDirectoryStub: &tenantDirectoryStub{byIDErr: activationErr}, revision: 4}

	assembly, err := AssembleTenantRuntime(TenantRuntimeAssemblyConfig{
		Runtimes: []*knet.LoadedSite{runtime}, Directory: directory, DirectoryRevision: 4,
		DirectoryConsumerID: "engine:test", RetirementLimit: 4,
		Common: &site.CommonConfig{}, Resources: resources,
		Services: TenantServiceConfig{ProcessContext: context.Background(), ResolveStorage: func(*site.Config) (storage.Backend, error) {
			return nil, nil
		}},
	})
	if assembly != nil || !errors.Is(err, activationErr) {
		t.Fatalf("AssembleTenantRuntime = (%v, %v), want activation failure", assembly, err)
	}
	if resources.Tenants == nil || resources.TenantCoordinator == nil || len(resources.UnregisteredTenants) != 0 {
		t.Fatalf("failed assembly ownership = tenants %v coordinator %v unregistered %d", resources.Tenants, resources.TenantCoordinator, len(resources.UnregisteredTenants))
	}
	if err := resources.Shutdown(context.Background()); err != nil {
		t.Fatalf("rollback partially assembled tenant runtime: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("tenant runtime DB was not closed by rollback owner:", err)
	}
}

func TestTenantRuntimeAssemblyDerivesActivationAdmissionFromResolvedTenant(t *testing.T) {
	platformDB, _, err := sqlmock.New()
	if err != nil {
		t.Fatal("create platform DB mock:", err)
	}
	defer platformDB.Close()
	var admitted *site.Config
	assembly, err := AssembleTenantRuntime(TenantRuntimeAssemblyConfig{
		Directory: &assemblyTestDirectory{tenantDirectoryStub: &tenantDirectoryStub{
			descriptors: []contract.SiteDescriptor{{SiteID: "tenant-activate", Status: "active"}},
			revision:    1,
		}},
		DirectoryConsumerID: "engine:admission", RetirementLimit: 1,
		Common:    &site.CommonConfig{DBHost: "platform-db", DBPort: 3306, DBUser: "default"},
		Resources: &ProcessResources{PlatformDB: platformDB, PlatformDialect: db.Resolve("mysql")},
		TenantActivationWorkAdmission: func(_ context.Context, config *site.Config) (func(), error) {
			admitted = config
			return func() {}, nil
		},
		Services: TenantServiceConfig{ProcessContext: context.Background(), Analytics: &analytics.Config{WALDir: t.TempDir()}, ResolveStorage: func(*site.Config) (storage.Backend, error) { return nil, nil }},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := assembly.Coordinator.WaitRetired(context.Background()); err != nil {
			t.Errorf("wait for retired test runtimes: %v", err)
		}
	}()
	info := site.DBSiteInfo{SiteID: "tenant-activate", Name: "activate.example", DBType: "postgres", DBHost: "tenant-db", DBPort: 5433, DBName: "tenant_db", DBUser: "tenant-user", DBPassword: "secret"}
	release, err := assembly.Coordinator.acquireActivationWork(context.Background(), info)
	if err != nil {
		t.Fatal("acquire activation work:", err)
	}
	release()
	if admitted == nil || admitted.DBType != "postgres" || admitted.DBHost != "tenant-db" || admitted.DBPort != 5433 || admitted.DBUser != "tenant-user" || admitted.DBPassword != "secret" {
		t.Fatalf("activation admission config = %+v, want resolved tenant endpoint", admitted)
	}
}
