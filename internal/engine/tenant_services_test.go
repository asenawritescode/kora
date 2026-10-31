package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/eventstream"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/site"
	"github.com/asenawritescode/kora/storage"
	"github.com/asenawritescode/kora/webhook"
)

func TestNewTenantServiceInstallerRequiresConcreteDependencies(t *testing.T) {
	if _, err := NewTenantServiceInstaller(TenantServiceConfig{}); err == nil {
		t.Fatal("expected missing common configuration to be rejected")
	}
	if _, err := NewTenantServiceInstaller(TenantServiceConfig{Common: &site.CommonConfig{}}); err == nil {
		t.Fatal("expected missing storage resolver to be rejected")
	}
}

func TestRegisterWebhookDeliveryDoesNotStartLossyListenerWhenOutboxDisabled(t *testing.T) {
	bus := &webhookListenerProbe{}
	worker := webhook.NewWorker(nil, bus, "tenant")
	defer worker.Stop()
	var starts atomic.Int32
	err := registerWebhookDelivery(TenantServiceConfig{
		OutboxEnabled: false,
		RegisterWorkerStart: func(start func() error) error {
			starts.Add(1)
			return start()
		},
	}, &knet.LoadedSite{Name: "tenant"}, worker, nil, nil)
	if err != nil {
		t.Fatalf("register disabled webhook delivery: %v", err)
	}
	if starts.Load() != 0 {
		t.Fatalf("worker starts = %d, want 0 when event integrations are disabled", starts.Load())
	}
	if bus.adds.Load() != 0 {
		t.Fatalf("live listeners registered = %d, want 0 for disabled outbox", bus.adds.Load())
	}
}

type webhookListenerProbe struct{ adds atomic.Int32 }

func (b *webhookListenerProbe) AddListener(chan eventstream.ChangeEvent)    { b.adds.Add(1) }
func (b *webhookListenerProbe) RemoveListener(chan eventstream.ChangeEvent) {}

func TestTenantServiceInstallerReusesStartupStorage(t *testing.T) {
	existing := &testStorageBackend{}
	var resolverCalls atomic.Int32
	got, err := resolveTenantStorage(existing, func(*site.SiteConfig) (storage.Backend, error) {
		resolverCalls.Add(1)
		return nil, errors.New("must not resolve storage twice")
	}, &site.SiteConfig{})
	if err != nil {
		t.Fatalf("resolve existing storage: %v", err)
	}
	if got != existing {
		t.Fatal("installer replaced the startup-owned storage backend")
	}
	if resolverCalls.Load() != 0 {
		t.Fatalf("storage resolver calls = %d, want 0", resolverCalls.Load())
	}
}

func TestTenantServiceInstallerPublishesCandidateBundleBeforeFailure(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create candidate database:", err)
	}
	resolveErr := errors.New("storage unavailable")
	install, err := NewTenantServiceInstaller(TenantServiceConfig{
		Common: &site.CommonConfig{DBType: "mysql"},
		ResolveStorage: func(*site.SiteConfig) (storage.Backend, error) {
			return nil, resolveErr
		},
	})
	if err != nil {
		t.Fatal("create tenant service installer:", err)
	}
	runtime := &knet.LoadedSite{Name: "tenant", DB: database, SessionManager: auth.NewOwnedSessionManager(database)}
	err = install(site.DBSiteInfo{SiteID: "tenant", Name: "tenant", DBType: "mysql"}, runtime)
	if !errors.Is(err, resolveErr) {
		t.Fatalf("install error = %v, want storage resolver error", err)
	}
	if runtime.RuntimeServices == nil {
		t.Fatal("candidate did not publish its partial service bundle before installation")
	}
	if runtime.SessionManager == nil {
		t.Fatal("candidate lost its owned session manager during failed installation")
	}
	mock.ExpectClose()
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("close failed candidate: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("candidate cleanup expectations:", err)
	}
}

type testStorageBackend struct{ storage.Backend }
