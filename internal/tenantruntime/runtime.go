package tenantruntime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/auth"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/eventstream"
)

// SiteConfig is the immutable tenant-facing configuration required by
// request routing and runtime resource setup.
type SiteConfig struct {
	Hostname      string
	Domains       []string
	FileStorage   string
	StorageBucket string
}

// Runtime owns the resources and worker generation for one activated tenant.
// Directory/router packages may publish a pointer to it, but lifecycle stays
// inside this package.
type Runtime struct {
	SiteID            string
	ConfigRevision    uint64
	DirectoryRevision uint64
	DatabaseIdentity  string
	RuntimeGeneration uint64
	Status            string
	DBType            string
	Name              string
	Config            SiteConfig
	DB                *sql.DB
	Registry          *doctype.Registry
	SessionManager    *auth.SessionManager
	AnalyticsEventBus eventstream.Bus
	AnalyticsWorker   interface {
		StartAsyncContext(context.Context)
		Stop()
		InvalidateMetrics(string)
		InvalidateAllMetrics()
		Flush() error
	}
	RuntimeServices *Services
	workerMu        sync.Mutex
	generation      *Generation
	lastUsedAt      atomic.Int64
}

type Activation struct {
	Tenant            contract.TenantID
	ConfigRevision    contract.Revision
	DirectoryRevision uint64
	ResourceRevision  contract.Revision
}

// Snapshot is an immutable, request-pinned view of one tenant activation.
type Snapshot struct {
	Activation      Activation
	Generation      uint64
	ConfigRevision  uint64
	Name            string
	Status          string
	DBType          string
	Config          SiteConfig
	DB              *sql.DB
	Registry        *doctype.Registry
	SessionManager  *auth.SessionManager
	AnalyticsWorker interface {
		StartAsyncContext(context.Context)
		Stop()
		InvalidateMetrics(string)
		InvalidateAllMetrics()
		Flush() error
	}
	Services    Services
	HasServices bool
}

func (s *Runtime) Snapshot() *Snapshot {
	if s == nil {
		return nil
	}
	config := s.Config
	config.Domains = append([]string(nil), s.Config.Domains...)
	return &Snapshot{
		Activation: Activation{
			Tenant:            contract.TenantID(s.SiteID),
			ConfigRevision:    contract.Revision(strconv.FormatUint(s.ConfigRevision, 10)),
			DirectoryRevision: s.DirectoryRevision,
			ResourceRevision:  contract.Revision(s.DatabaseIdentity),
		},
		Generation: s.EnsureRuntimeGeneration(), ConfigRevision: s.ConfigRevision,
		Name: s.Name, Status: s.Status, DBType: s.DBType,
		Config: config, DB: s.DB, Registry: s.Registry, SessionManager: s.SessionManager,
		AnalyticsWorker: s.AnalyticsWorker, Services: s.RuntimeServiceSnapshot(), HasServices: s.RuntimeServices != nil,
	}
}

func (s *Runtime) WorkerContext(parent context.Context) context.Context {
	if s == nil {
		return context.Background()
	}
	return s.ensureGeneration(parent).WorkerContext()
}

func (s *Runtime) StartWorker(parent context.Context, run func(context.Context)) bool {
	if s == nil {
		return false
	}
	return s.ensureGeneration(parent).StartWorker(run)
}

func (s *Runtime) StopWorkers() {
	_ = s.StopWorkersContext(context.Background())
}

func (s *Runtime) StopWorkersContext(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.workerMu.Lock()
	generation := s.generation
	s.workerMu.Unlock()
	return generation.StopWorkersContext(ctx)
}

func (s *Runtime) ensureGeneration(parent context.Context) *Generation {
	s.workerMu.Lock()
	defer s.workerMu.Unlock()
	if s.generation == nil {
		s.generation = NewGeneration(s.EnsureRuntimeGeneration(), parent)
	}
	return s.generation
}

func (s *Runtime) AcquireRuntime() (*Handle, bool) {
	if s == nil {
		return nil, false
	}
	handle, ok := s.ensureGeneration(context.Background()).Acquire()
	if ok {
		s.lastUsedAt.Store(time.Now().UnixNano())
		handle.snapshot = s.Snapshot()
	}
	return handle, ok
}

func (s *Runtime) lastUsedUnixNano() int64 {
	if s == nil {
		return 0
	}
	return s.lastUsedAt.Load()
}

func (s *Runtime) retireIfIdle() bool {
	if s == nil {
		return false
	}
	return s.ensureGeneration(context.Background()).RetireIfIdle()
}

func (s *Runtime) RetireRuntime() {
	if s != nil {
		s.ensureGeneration(context.Background()).Retire()
	}
}

var runtimeGenerationCounter atomic.Uint64

func (s *Runtime) EnsureRuntimeGeneration() uint64 {
	if s == nil {
		return 0
	}
	if generation := atomic.LoadUint64(&s.RuntimeGeneration); generation != 0 {
		return generation
	}
	generation := runtimeGenerationCounter.Add(1)
	if atomic.CompareAndSwapUint64(&s.RuntimeGeneration, 0, generation) {
		return generation
	}
	return atomic.LoadUint64(&s.RuntimeGeneration)
}

func (s *Runtime) CloseSessionManager() {
	if s != nil && s.SessionManager != nil {
		_ = s.SessionManager.Close()
	}
}

// Close retires the runtime, joins its workers and admitted requests, then
// closes tenant-owned dependencies in reverse-use order. A drain timeout leaves
// resources open so callers may retry without invalidating active work.
func (s *Runtime) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.BeginDrain()
	if err := s.StopWorkersContext(ctx); err != nil {
		return err
	}
	if err := s.WaitForRequests(ctx); err != nil {
		return err
	}
	var closeErr error
	if s.AnalyticsWorker != nil {
		if err := stopWorkerWithContext(ctx, "analytics worker", s.AnalyticsWorker.Stop); err != nil {
			return err
		}
	}
	if s.SessionManager != nil {
		var sessionCloseErr error
		if err := runCleanupWithContext(ctx, "tenant session manager", func() { sessionCloseErr = s.SessionManager.Close() }); err != nil {
			return errors.Join(closeErr, err)
		}
		closeErr = errors.Join(closeErr, sessionCloseErr)
	}
	services := s.RuntimeServiceSnapshot()
	if services.WebhookWorker != nil {
		if err := stopWorkerWithContext(ctx, "webhook worker", services.WebhookWorker.Stop); err != nil {
			return errors.Join(closeErr, err)
		}
	}
	if services.AnalyticsRelay != nil {
		if err := stopWorkerWithContext(ctx, "analytics relay", services.AnalyticsRelay.Stop); err != nil {
			return errors.Join(closeErr, err)
		}
	}
	if bus, ok := services.EventBus.(interface{ Close() error }); ok {
		var busCloseErr error
		if err := runCleanupWithContext(ctx, "tenant event bus", func() { busCloseErr = bus.Close() }); err != nil {
			return errors.Join(closeErr, err)
		}
		closeErr = errors.Join(closeErr, busCloseErr)
	}
	if s.AnalyticsEventBus != nil && services.EventBus == nil {
		var busCloseErr error
		if err := runCleanupWithContext(ctx, "tenant analytics bus", func() { busCloseErr = s.AnalyticsEventBus.Close() }); err != nil {
			return errors.Join(closeErr, err)
		}
		closeErr = errors.Join(closeErr, busCloseErr)
	}
	if closer, ok := services.Storage.(io.Closer); ok {
		closeErr = errors.Join(closeErr, closer.Close())
	}
	if s.DB != nil {
		closeErr = errors.Join(closeErr, s.DB.Close())
	}
	return closeErr
}

func stopWorkerWithContext(ctx context.Context, name string, stop func()) error {
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

func runCleanupWithContext(ctx context.Context, name string, cleanup func()) error {
	done := make(chan struct{})
	go func() {
		cleanup()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("clean up %s: %w", name, ctx.Err())
	}
}

func (s *Runtime) RuntimeServiceSnapshot() Services {
	if s == nil || s.RuntimeServices == nil {
		return Services{}
	}
	return *s.RuntimeServices
}

func (s *Runtime) BeginDrain() <-chan struct{} {
	s.RetireRuntime()
	s.workerMu.Lock()
	generation := s.generation
	s.workerMu.Unlock()
	return generation.Drained()
}

func (s *Runtime) WaitForRequests(ctx context.Context) error {
	if s == nil {
		return nil
	}
	return s.ensureGeneration(context.Background()).Wait(ctx)
}

func (s *Runtime) WaitForIdle(ctx context.Context) error {
	if s == nil {
		return nil
	}
	return s.ensureGeneration(context.Background()).WaitIdle(ctx)
}
