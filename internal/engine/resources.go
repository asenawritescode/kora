package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/db"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/scheduler"
	"github.com/asenawritescode/kora/script"
)

// ShutdownOwner is the process resource boundary owned by Engine. Shutdown
// must stop producers and join users before closing the resources they use.
type ShutdownOwner interface {
	Shutdown(context.Context) error
}

// ProcessResources is the explicit process-level ownership set registered
// during startup and handed to Engine for normal shutdown. It also owns startup
// rollback before that transfer completes. Tenant-scoped services remain
// owned by each LoadedSite generation.
type ProcessResources struct {
	workerStartMu           sync.Mutex
	workerStarts            []func() error
	workersStarted          bool
	shutdownStarted         bool
	workerStartWG           sync.WaitGroup
	unregisteredMu          sync.Mutex
	StopAsyncHookWorker     func()
	CancelAsyncHookWorker   context.CancelFunc
	AsyncHookWorkerDone     <-chan struct{}
	CancelProcess           context.CancelFunc
	CloseRealtimeStreams    func()
	SystemGuard             *auth.SystemGuard
	CloseOnboardingJobs     func(context.Context) error
	Scheduler               *scheduler.Scheduler
	DeletionWorker          *PendingDeletionWorker
	DirectoryLagMonitor     *DirectoryLagMonitor
	DirectoryFeed           *DirectoryFeedWorker
	DurableWorkScanner      *DurableWorkScanner
	TenantCoordinator       *TenantRuntimeCoordinator
	UnregisteredTenants     []*knet.LoadedSite
	Tenants                 *knet.SiteRouter
	SessionManager          *auth.SessionManager
	ManagementWorkerDone    <-chan struct{}
	ScriptRunner            script.Runner
	NATSProvider            interface{ Close() }
	PlatformDB              *sql.DB
	PlatformDialect         db.Dialect
	TelemetryShutdown       func(context.Context) error
	ResourceMetrics         *ResourceMetrics
	RealtimeStreams         *RealtimeStreamAdmission
	SessionCacheBudget      *auth.SessionCacheBudget
	UploadAdmission         *UploadAdmission
	AnalyticsQueueAdmission *analytics.AnalyticsQueueAdmission
}

// RegisterWorkerStart defers worker execution until Engine.Run begins. If the
// process is already running, newly activated tenant generations start at once.
func (r *ProcessResources) RegisterWorkerStart(start func() error) error {
	if r == nil || start == nil {
		return errors.New("worker start registration requires an owner and function")
	}
	r.workerStartMu.Lock()
	if r.shutdownStarted {
		r.workerStartMu.Unlock()
		return errors.New("Engine process is shutting down")
	}
	if r.workersStarted {
		r.workerStartWG.Add(1)
		r.workerStartMu.Unlock()
		defer r.workerStartWG.Done()
		return start()
	}
	r.workerStarts = append(r.workerStarts, start)
	r.workerStartMu.Unlock()
	return nil
}

// StartRegisteredWorkers starts assembly-time registrations exactly once and
// before the listener can publish readiness.
func (r *ProcessResources) StartRegisteredWorkers() error {
	if r == nil {
		return nil
	}
	r.workerStartMu.Lock()
	if r.shutdownStarted {
		r.workerStartMu.Unlock()
		return errors.New("Engine process is shutting down")
	}
	if r.workersStarted {
		r.workerStartMu.Unlock()
		return nil
	}
	r.workersStarted = true
	starts := r.workerStarts
	r.workerStarts = nil
	r.workerStartWG.Add(len(starts))
	r.workerStartMu.Unlock()
	var startErr error
	for index, start := range starts {
		if err := start(); err != nil {
			startErr = errors.Join(startErr, fmt.Errorf("start registered Engine worker %d: %w", index+1, err))
		}
		r.workerStartWG.Done()
	}
	return startErr
}

// RetainUnregisteredTenant transfers a failed-to-publish runtime candidate to
// the process owner so startup rollback/shutdown can retry its close.
func (r *ProcessResources) RetainUnregisteredTenant(runtime *knet.LoadedSite) {
	if r == nil || runtime == nil {
		return
	}
	r.unregisteredMu.Lock()
	defer r.unregisteredMu.Unlock()
	for _, existing := range r.UnregisteredTenants {
		if existing == runtime {
			return
		}
	}
	r.UnregisteredTenants = append(r.UnregisteredTenants, runtime)
}

// Shutdown stops process producers, joins their goroutines, and closes shared
// resources in dependency order. If a bounded worker join expires, dependent
// resources are retained rather than being closed beneath a live goroutine.
func (r *ProcessResources) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.workerStartMu.Lock()
	r.shutdownStarted = true
	r.workerStarts = nil
	r.workerStartMu.Unlock()
	if r.CancelProcess != nil {
		r.CancelProcess()
	}
	// WebSocket upgrades are hijacked connections and are outside
	// http.Server.Shutdown's connection set. Close them before waiting for
	// tenant generation users to drain so runtime retirement cannot hang behind
	// an indefinitely live stream.
	if r.CloseRealtimeStreams != nil {
		r.CloseRealtimeStreams()
	}
	if err := waitForGroup(ctx, "registered worker startup", &r.workerStartWG); err != nil {
		return incompleteShutdown(err)
	}
	if r.ManagementWorkerDone != nil {
		if err := waitForWorker(ctx, "management operation worker", r.ManagementWorkerDone); err != nil {
			return incompleteShutdown(err)
		}
	}
	if r.DurableWorkScanner != nil {
		if err := r.DurableWorkScanner.Shutdown(ctx); err != nil {
			return incompleteShutdown(fmt.Errorf("join durable tenant work scanner: %w", err))
		}
	}
	if r.DirectoryLagMonitor != nil {
		if err := r.DirectoryLagMonitor.Shutdown(ctx); err != nil {
			return incompleteShutdown(err)
		}
	}
	if r.SystemGuard != nil {
		if err := stopWithContext(ctx, "system session cleanup", func() { _ = r.SystemGuard.Close() }); err != nil {
			return incompleteShutdown(err)
		}
	}
	if r.CloseOnboardingJobs != nil {
		if err := r.CloseOnboardingJobs(ctx); err != nil {
			return incompleteShutdown(fmt.Errorf("join onboarding provisioning jobs: %w", err))
		}
	}
	if r.Scheduler != nil {
		if err := r.Scheduler.StopContext(ctx); err != nil {
			return incompleteShutdown(fmt.Errorf("join scheduler: %w", err))
		}
	}
	if r.DeletionWorker != nil {
		if err := r.DeletionWorker.Shutdown(ctx); err != nil {
			return incompleteShutdown(err)
		}
	}
	if r.DirectoryFeed != nil {
		if err := r.DirectoryFeed.Shutdown(ctx); err != nil {
			return incompleteShutdown(err)
		}
	}
	if r.Tenants != nil {
		for _, loaded := range r.Tenants.AllSites() {
			services := loaded.RuntimeServiceSnapshot()
			if services.WebhookWorker != nil {
				if err := stopWithContext(ctx, "webhook worker", services.WebhookWorker.Stop); err != nil {
					return incompleteShutdown(err)
				}
			}
			if services.AnalyticsRelay != nil {
				if err := stopWithContext(ctx, "analytics relay", services.AnalyticsRelay.Stop); err != nil {
					return incompleteShutdown(err)
				}
			}
			if err := loaded.StopWorkersContext(ctx); err != nil {
				return incompleteShutdown(fmt.Errorf("stop tenant workers for %q: %w", loaded.SiteID, err))
			}
			if loaded.AnalyticsWorker != nil {
				if err := stopWithContext(ctx, "analytics worker", loaded.AnalyticsWorker.Stop); err != nil {
					return incompleteShutdown(err)
				}
			}
		}
	}
	// Stop producers before closing the queue. The hook worker has an
	// independent context so it can drain accepted work while tenant pools and
	// script execution remain available.
	if r.StopAsyncHookWorker != nil {
		r.StopAsyncHookWorker()
	}
	if r.AsyncHookWorkerDone != nil {
		if err := waitForWorker(ctx, "async script hook worker", r.AsyncHookWorkerDone); err != nil {
			if r.CancelAsyncHookWorker != nil {
				r.CancelAsyncHookWorker()
			}
			return incompleteShutdown(err)
		}
	}
	if r.CancelAsyncHookWorker != nil {
		r.CancelAsyncHookWorker()
	}
	if r.SessionManager != nil {
		if err := r.SessionManager.Close(); err != nil {
			return incompleteShutdown(fmt.Errorf("close process session manager: %w", err))
		}
	}
	var unregisteredCloseErr error
	r.unregisteredMu.Lock()
	unregisteredTenants := append([]*knet.LoadedSite(nil), r.UnregisteredTenants...)
	r.unregisteredMu.Unlock()
	for _, runtime := range unregisteredTenants {
		if runtime == nil {
			continue
		}
		if err := runtime.Close(ctx); err != nil {
			unregisteredCloseErr = errors.Join(unregisteredCloseErr, fmt.Errorf("close startup tenant runtime %q: %w", runtime.Name, err))
		}
	}
	if unregisteredCloseErr != nil {
		return incompleteShutdown(unregisteredCloseErr)
	}
	if r.TenantCoordinator != nil {
		if err := r.TenantCoordinator.WaitRetired(ctx); err != nil {
			return incompleteShutdown(err)
		}
	}
	if r.ScriptRunner != nil {
		if err := r.ScriptRunner.Close(); err != nil {
			return incompleteShutdown(fmt.Errorf("close script runner: %w", err))
		}
	}
	if r.Tenants != nil {
		if err := r.Tenants.RuntimeManager().Close(ctx); err != nil {
			return incompleteShutdown(fmt.Errorf("close tenant runtime manager; shared resources remain available for process exit: %w", err))
		}
	}
	if r.NATSProvider != nil {
		r.NATSProvider.Close()
	}
	var closeErr error
	if r.PlatformDB != nil {
		if err := r.PlatformDB.Close(); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("close platform database: %w", err))
		}
	}
	if r.ResourceMetrics != nil {
		if err := r.ResourceMetrics.Unregister(); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("unregister Engine resource metrics: %w", err))
		}
	}
	if r.TelemetryShutdown != nil {
		telemetryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := r.TelemetryShutdown(telemetryCtx); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("shut down telemetry: %w", err))
		}
		cancel()
	}
	if closeErr != nil {
		return incompleteShutdown(closeErr)
	}
	return nil
}

func incompleteShutdown(err error) error {
	return errors.Join(ErrShutdownIncomplete, err)
}

func waitForGroup(ctx context.Context, name string, group *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("join %s: %w", name, ctx.Err())
	}
}

func waitForWorker(ctx context.Context, name string, done <-chan struct{}) error {
	if done == nil {
		return fmt.Errorf("join %s: completion channel is nil", name)
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("join %s: %w", name, ctx.Err())
	}
}

func stopWithContext(ctx context.Context, name string, stop func()) error {
	if stop == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stop %s: %w", name, ctx.Err())
	}
}
