package engine

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/auth"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/script"
	"github.com/asenawritescode/kora/site"
	"github.com/asenawritescode/kora/storage"
	"go.opentelemetry.io/otel"
)

func TestNewRollsBackEarlierStagesWhenSharedRuntimeFails(t *testing.T) {
	wantErr := errors.New("injected shared runtime failure")
	var cleanup []string
	deps := engineAssemblyDependencies{
		assembleStartup: func(ctx context.Context, _ ProcessStartupConfig, resources *ProcessResources) (*ProcessStartupAssembly, error) {
			resources.TelemetryShutdown = func(context.Context) error {
				cleanup = append(cleanup, "telemetry")
				return nil
			}
			startupCtx, span := otel.Tracer("test").Start(ctx, "startup")
			return &ProcessStartupAssembly{
				Context: startupCtx, Span: span, Started: time.Now(),
				Platform: PlatformSiteDirectory{}, Tenants: StartupTenants{},
			}, nil
		},
		sharedRuntime: func(context.Context, SharedRuntimeConfig, *ProcessResources) (*SharedRuntime, error) {
			return nil, wantErr
		},
		assembleTenants: func(TenantRuntimeAssemblyConfig) (*TenantRuntimeAssembly, error) {
			t.Fatal("tenant assembly ran after shared runtime failure")
			return nil, nil
		},
		assembleHTTP: func(HTTPProcessAssemblyConfig) (*HTTPProcessAssembly, error) {
			t.Fatal("HTTP assembly ran after shared runtime failure")
			return nil, nil
		},
	}
	config := Config{
		Common:         &site.CommonConfig{},
		ResolveStorage: func(*site.SiteConfig) (storage.Backend, error) { return nil, nil },
	}
	engine, err := newEngine(context.Background(), config, deps)
	if engine != nil || !errors.Is(err, wantErr) {
		t.Fatalf("newEngine = (%v, %v), want nil and injected error", engine, err)
	}
	if len(cleanup) != 1 || cleanup[0] != "telemetry" {
		t.Fatalf("startup rollback = %v, want [telemetry]", cleanup)
	}
}

func TestNewRejectsInvalidSharedSessionCacheBudgetBeforeAcquisition(t *testing.T) {
	called := false
	deps := engineAssemblyDependencies{
		assembleStartup: func(context.Context, ProcessStartupConfig, *ProcessResources) (*ProcessStartupAssembly, error) {
			called = true
			return nil, errors.New("startup should not run")
		},
		sharedRuntime: func(context.Context, SharedRuntimeConfig, *ProcessResources) (*SharedRuntime, error) {
			t.Fatal("shared runtime should not run")
			return nil, nil
		},
		assembleTenants: func(TenantRuntimeAssemblyConfig) (*TenantRuntimeAssembly, error) {
			t.Fatal("tenant assembly should not run")
			return nil, nil
		},
		assembleHTTP: func(HTTPProcessAssemblyConfig) (*HTTPProcessAssembly, error) {
			t.Fatal("HTTP assembly should not run")
			return nil, nil
		},
	}
	_, err := newEngine(context.Background(), Config{
		Common: &site.CommonConfig{}, ResolveStorage: func(*site.SiteConfig) (storage.Backend, error) { return nil, nil },
		SessionCacheBudgetBytes: auth.MaxSessionCacheBudgetBytes + 1,
	}, deps)
	if err == nil {
		t.Fatal("invalid aggregate session-cache budget was accepted")
	}
	if called {
		t.Fatal("process resources were acquired before session-cache budget validation")
	}
}

func TestNewRejectsUnsafePublicAppBaseURLBeforeAcquisition(t *testing.T) {
	called := false
	deps := engineAssemblyDependencies{
		assembleStartup: func(context.Context, ProcessStartupConfig, *ProcessResources) (*ProcessStartupAssembly, error) {
			called = true
			t.Fatal("startup should not run with an unsafe public app origin")
			return nil, nil
		},
		sharedRuntime: func(context.Context, SharedRuntimeConfig, *ProcessResources) (*SharedRuntime, error) {
			t.Fatal("shared runtime should not run")
			return nil, nil
		},
		assembleTenants: func(TenantRuntimeAssemblyConfig) (*TenantRuntimeAssembly, error) {
			t.Fatal("tenant assembly should not run")
			return nil, nil
		},
		assembleHTTP: func(HTTPProcessAssemblyConfig) (*HTTPProcessAssembly, error) {
			t.Fatal("HTTP assembly should not run")
			return nil, nil
		},
	}
	_, err := newEngine(context.Background(), Config{
		Common: &site.CommonConfig{}, ResolveStorage: func(*site.SiteConfig) (storage.Backend, error) { return nil, nil },
		PublicAppBaseURL: "http://app.example.test",
	}, deps)
	if err == nil {
		t.Fatal("unsafe public app base URL was accepted")
	}
	if called {
		t.Fatal("process resources were acquired before public app origin validation")
	}
}

func TestNewUsesPlatformDatabaseForApplicationFallbacks(t *testing.T) {
	checkpointVerifier := &managementCheckpointVerifierStub{}
	platformDB, platformMock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create platform database:", err)
	}
	tenantDB, tenantMock, err := sqlmock.New()
	if err != nil {
		_ = platformDB.Close()
		t.Fatal("create tenant database:", err)
	}
	platformMock.ExpectClose()
	tenantMock.ExpectClose()
	wantErr := errors.New("stop after verifying HTTP composition")
	deps := engineAssemblyDependencies{
		assembleStartup: func(ctx context.Context, _ ProcessStartupConfig, resources *ProcessResources) (*ProcessStartupAssembly, error) {
			resources.PlatformDB = platformDB
			startupCtx, span := otel.Tracer("test").Start(ctx, "startup")
			return &ProcessStartupAssembly{
				Context: startupCtx, Span: span, Started: time.Now(),
				Platform: PlatformSiteDirectory{DB: platformDB},
				Tenants:  StartupTenants{PrimaryDB: tenantDB},
			}, nil
		},
		sharedRuntime: func(context.Context, SharedRuntimeConfig, *ProcessResources) (*SharedRuntime, error) {
			return &SharedRuntime{}, nil
		},
		assembleTenants: func(config TenantRuntimeAssemblyConfig) (*TenantRuntimeAssembly, error) {
			if config.Ingress != nil {
				t.Errorf("single-deployment startup passed a non-nil ingress directory: %T", config.Ingress)
			}
			router := knet.NewSiteRouter(nil)
			config.Resources.Tenants = router
			return &TenantRuntimeAssembly{Tenants: router}, nil
		},
		assembleHTTP: func(config HTTPProcessAssemblyConfig) (*HTTPProcessAssembly, error) {
			if config.PlatformRoutes.ManagementCheckpointVerifier != checkpointVerifier {
				t.Errorf("management checkpoint verifier = %T, want injected verifier %T", config.PlatformRoutes.ManagementCheckpointVerifier, checkpointVerifier)
			}
			if config.Application.Database != platformDB {
				t.Errorf("application fallback database = %p, want platform DB %p", config.Application.Database, platformDB)
			}
			if config.PlatformRoutes.PrimaryDB != nil {
				t.Errorf("platform primary database is tenant-bound: %p", config.PlatformRoutes.PrimaryDB)
			}
			if config.PlatformRoutes.PlatformDB != platformDB {
				t.Errorf("platform health database = %p, want platform DB %p", config.PlatformRoutes.PlatformDB, platformDB)
			}
			return nil, wantErr
		},
	}
	_, err = newEngine(context.Background(), Config{
		Common: &site.CommonConfig{}, ResolveStorage: func(*site.SiteConfig) (storage.Backend, error) { return nil, nil },
		ManagementCheckpointVerifier: checkpointVerifier,
	}, deps)
	if !errors.Is(err, wantErr) {
		t.Fatalf("newEngine error = %v, want injected HTTP composition failure", err)
	}
	if err := platformMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("platform DB cleanup expectations: %v", err)
	}
	if err := tenantDB.Close(); err != nil {
		t.Fatalf("close unowned test tenant DB: %v", err)
	}
	if err := tenantMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("tenant DB test cleanup expectations: %v", err)
	}
}

type rollbackTestRunner struct{ close func() }

func (r rollbackTestRunner) Execute(context.Context, script.ExecuteRequest) (*script.ExecuteResult, error) {
	return nil, nil
}
func (rollbackTestRunner) Validate(string) error { return nil }
func (r rollbackTestRunner) Close() error {
	if r.close != nil {
		r.close()
	}
	return nil
}

func TestNewRollsBackResourcesAtLaterAssemblyFailures(t *testing.T) {
	for _, stage := range []string{"startup", "tenants", "http"} {
		t.Run(stage, func(t *testing.T) {
			wantErr := errors.New("injected " + stage + " failure")
			var cleanup []string
			var listenerDomains []string
			var reloadToken string
			deps := engineAssemblyDependencies{
				assembleStartup: func(ctx context.Context, _ ProcessStartupConfig, resources *ProcessResources) (*ProcessStartupAssembly, error) {
					resources.TelemetryShutdown = func(context.Context) error { cleanup = append(cleanup, "telemetry"); return nil }
					if stage == "startup" {
						return nil, wantErr
					}
					startupCtx, span := otel.Tracer("test").Start(ctx, "startup")
					return &ProcessStartupAssembly{
						Context: startupCtx, Span: span, Started: time.Now(),
						Tenants: StartupTenants{Domains: []string{"tenant.example"}},
					}, nil
				},
				sharedRuntime: func(_ context.Context, _ SharedRuntimeConfig, resources *ProcessResources) (*SharedRuntime, error) {
					runner := rollbackTestRunner{close: func() { cleanup = append(cleanup, "script") }}
					resources.ScriptRunner = runner
					return &SharedRuntime{ScriptRunner: runner}, nil
				},
				assembleTenants: func(config TenantRuntimeAssemblyConfig) (*TenantRuntimeAssembly, error) {
					if stage == "tenants" {
						return nil, wantErr
					}
					router := knet.NewSiteRouter(nil)
					config.Resources.Tenants = router
					return &TenantRuntimeAssembly{Tenants: router}, nil
				},
				assembleHTTP: func(config HTTPProcessAssemblyConfig) (*HTTPProcessAssembly, error) {
					listenerDomains = append(listenerDomains, config.Listener.TLS.Domains...)
					reloadToken = config.ReloadToken
					return nil, wantErr
				},
			}
			config := Config{
				Common: &site.CommonConfig{}, Listener: HTTPServerConfig{TLS: knet.TLSConfig{Mode: "auto"}},
				ReloadToken:    "test-reload-token",
				ResolveStorage: func(*site.SiteConfig) (storage.Backend, error) { return nil, nil },
			}
			engine, err := newEngine(context.Background(), config, deps)
			if engine != nil || !errors.Is(err, wantErr) {
				t.Fatalf("newEngine = (%v, %v), want nil and %v", engine, err, wantErr)
			}
			wantCleanup := []string{"telemetry"}
			if stage == "tenants" || stage == "http" {
				wantCleanup = []string{"script", "telemetry"}
			}
			if len(cleanup) != len(wantCleanup) {
				t.Fatalf("rollback order = %v, want %v", cleanup, wantCleanup)
			}
			for index := range wantCleanup {
				if cleanup[index] != wantCleanup[index] {
					t.Fatalf("rollback order = %v, want %v", cleanup, wantCleanup)
				}
			}
			if stage == "http" && (len(listenerDomains) != 1 || listenerDomains[0] != "tenant.example") {
				t.Fatalf("autocert listener domains = %v, want [tenant.example]", listenerDomains)
			}
			if stage == "http" && reloadToken != "test-reload-token" {
				t.Fatalf("HTTP assembly reload token = %q, want configured token", reloadToken)
			}
		})
	}
}

func TestEngineRunStartsRegisteredWorkersBeforeReadiness(t *testing.T) {
	var starts atomic.Int32
	ready := make(chan struct{})
	deps := engineAssemblyDependencies{
		assembleStartup: func(ctx context.Context, _ ProcessStartupConfig, _ *ProcessResources) (*ProcessStartupAssembly, error) {
			startupCtx, span := otel.Tracer("test").Start(ctx, "startup")
			return &ProcessStartupAssembly{Context: startupCtx, Span: span, Started: time.Now()}, nil
		},
		sharedRuntime: func(context.Context, SharedRuntimeConfig, *ProcessResources) (*SharedRuntime, error) {
			return &SharedRuntime{}, nil
		},
		assembleTenants: func(config TenantRuntimeAssemblyConfig) (*TenantRuntimeAssembly, error) {
			router := knet.NewSiteRouter(nil)
			config.Resources.Tenants = router
			if err := config.Services.RegisterWorkerStart(func() error { starts.Add(1); return nil }); err != nil {
				return nil, err
			}
			return &TenantRuntimeAssembly{Tenants: router}, nil
		},
		assembleHTTP: func(HTTPProcessAssemblyConfig) (*HTTPProcessAssembly, error) {
			return &HTTPProcessAssembly{Server: knet.NewServer(http.NotFoundHandler(), "127.0.0.1:0", nil)}, nil
		},
	}
	config := Config{
		Common:         &site.CommonConfig{},
		ResolveStorage: func(*site.SiteConfig) (storage.Backend, error) { return nil, nil },
		SystemGuard:    auth.LoadSystemGuardFromEnvPaused(),
		OnReady:        func(EngineMetrics) { close(ready) },
	}
	engine, err := newEngine(context.Background(), config, deps)
	if err != nil {
		t.Fatal("newEngine:", err)
	}
	if starts.Load() != 0 {
		t.Fatal("Engine.New started a registered tenant worker")
	}
	runDone := make(chan error, 1)
	go func() { runDone <- engine.Run(context.Background()) }()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("Engine did not publish readiness")
	}
	if starts.Load() != 1 {
		t.Fatalf("worker start count at readiness = %d, want 1", starts.Load())
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := engine.Shutdown(shutdownCtx); err != nil {
		t.Fatal("Shutdown:", err)
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal("Run:", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Engine.Run did not join after shutdown")
	}
}
