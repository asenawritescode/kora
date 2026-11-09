package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/auth"
	"github.com/asenawritescode/kora/internal/eventstream"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
	"github.com/asenawritescode/kora/internal/webhook"
)

type tenantExtensionSchemas struct{ registered bool }

func (s tenantExtensionSchemas) Registered(contract.SchemaRef) bool               { return s.registered }
func (tenantExtensionSchemas) Validate(contract.SchemaRef, json.RawMessage) error { return nil }

func TestNewTenantServiceInstallerRequiresConcreteDependencies(t *testing.T) {
	if _, err := NewTenantServiceInstaller(TenantServiceConfig{}); err == nil {
		t.Fatal("expected missing common configuration to be rejected")
	}
	if _, err := NewTenantServiceInstaller(TenantServiceConfig{Common: &site.CommonConfig{}}); err == nil {
		t.Fatal("expected missing storage resolver to be rejected")
	}
}

func TestTenantExtensionActivationRequiresRegisteredSchemas(t *testing.T) {
	if err := validateModuleExtensionSchemas(nil, nil); err != nil {
		t.Fatalf("module set without extensions requires a schema registry: %v", err)
	}
	target := contract.ResourceRef{Namespace: "tenant", Name: "Invoice", Version: 1}
	handler := contract.ResourceRef{Namespace: "tenant", Name: "tax.validate", Version: 1}
	modules := []contract.ModuleDescriptor{
		{ID: "finance.ar", Version: "1.0.0", Resources: []contract.OwnedResource{{Ref: target}}, ExtensionPoints: []contract.ExtensionPoint{{OwnerModuleID: "finance.ar", ID: "invoice.validation", Version: 1, Target: target, AllowedKinds: []string{string(contract.ExtensionValidation)}, InputSchema: contract.SchemaRef{ID: "invoice", Version: 1}, OutputSchema: contract.SchemaRef{ID: "result", Version: 1}, ExecutionPhase: "before_commit", FailurePolicy: "reject"}}},
		{ID: "tax.vat", Version: "1.0.0", Resources: []contract.OwnedResource{{Ref: handler}}, Extensions: []contract.ExtensionRegistration{{ID: "vat-check", ExtenderModuleID: "tax.vat", PointID: "finance.ar/invoice.validation", PointVersion: 1, Kind: contract.ExtensionValidation, ArtifactDigest: strings.Repeat("d", 64), HandlerRef: handler, Order: 1}}},
	}
	if err := validateModuleExtensionSchemas(modules, nil); err == nil {
		t.Fatal("extension activation without a schema registry was accepted")
	}
	if err := validateModuleExtensionSchemas(modules, tenantExtensionSchemas{}); err == nil {
		t.Fatal("extension activation with unregistered schemas was accepted")
	}
	if err := validateModuleExtensionSchemas(modules, tenantExtensionSchemas{registered: true}); err != nil {
		t.Fatalf("registered extension schemas were rejected: %v", err)
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
	got, err := resolveTenantStorage(existing, func(*site.Config) (storage.Backend, error) {
		resolverCalls.Add(1)
		return nil, errors.New("must not resolve storage twice")
	}, &site.Config{})
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
		ResolveStorage: func(*site.Config) (storage.Backend, error) {
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
