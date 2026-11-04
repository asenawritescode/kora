package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asenawritescode/kora/internal/tenantruntime"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/site"
)

type TenantRuntimeInstaller func(site.DBSiteInfo, *knet.LoadedSite) error

// TenantRuntimeCoordinator owns cold activation, replacement, retirement and
// joined drain for tenant generations after the process manager is assembled.
type TenantRuntimeCoordinator struct {
	tenants                 *knet.SiteRouter
	directory               TenantRuntimeDirectory
	common                  *site.CommonConfig
	version                 string
	install                 TenantRuntimeInstaller
	ingress                 TenantIngressDirectory
	retain                  func(*knet.LoadedSite)
	drainTimeout            time.Duration
	maxResidentTenants      uint32
	operationGate           chan struct{}
	waitersMu               sync.Mutex
	waiters                 uint16
	maxWaiters              uint16
	operationWait           time.Duration
	activationWorkAdmission func(context.Context, site.DBSiteInfo) (func(), error)
	waitCount               atomic.Uint64
	waitNanos               atomic.Uint64
	rejected                atomic.Uint64
	retiredDrains           sync.WaitGroup
	managementRetireMu      sync.Mutex
	managementRetires       map[string]*tenantRetirement
	load                    func(context.Context, site.DBSiteInfo, *site.CommonConfig, *knet.LoadedSite, string) (*knet.LoadedSite, error)
}

type tenantRetirement struct {
	done chan struct{}
	err  error
}

type TenantRuntimeCoordinatorConfig struct {
	Tenants                 *knet.SiteRouter
	Directory               TenantRuntimeDirectory
	Common                  *site.CommonConfig
	BinaryVersion           string
	Installer               TenantRuntimeInstaller
	Ingress                 TenantIngressDirectory
	RetainRuntime           func(*knet.LoadedSite)
	DrainTimeout            time.Duration
	MaxResidentTenants      uint32
	MaxActivationWaiters    uint16
	ActivationWait          time.Duration
	ActivationWorkAdmission func(context.Context, site.DBSiteInfo) (func(), error)
}

func NewTenantRuntimeCoordinator(config TenantRuntimeCoordinatorConfig) (*TenantRuntimeCoordinator, error) {
	return newTenantRuntimeCoordinator(config, LoadTenantRuntime)
}

const DefaultMaxResidentTenants uint32 = 256
const DefaultMaxActivationWaiters uint16 = 32

func newTenantRuntimeCoordinator(config TenantRuntimeCoordinatorConfig, load func(context.Context, site.DBSiteInfo, *site.CommonConfig, *knet.LoadedSite, string) (*knet.LoadedSite, error)) (*TenantRuntimeCoordinator, error) {
	if config.Tenants == nil || config.Common == nil || config.Installer == nil || config.RetainRuntime == nil || load == nil {
		return nil, errors.New("tenant coordinator requires runtime manager, common config, installer, candidate owner and loader")
	}
	if config.DrainTimeout <= 0 {
		config.DrainTimeout = 30 * time.Second
	}
	if config.MaxResidentTenants == 0 {
		config.MaxResidentTenants = DefaultMaxResidentTenants
	}
	if config.ActivationWait <= 0 {
		config.ActivationWait = 1500 * time.Millisecond
	}
	if config.MaxActivationWaiters == 0 {
		config.MaxActivationWaiters = DefaultMaxActivationWaiters
	}
	return &TenantRuntimeCoordinator{
		tenants: config.Tenants, directory: config.Directory, common: config.Common, version: config.BinaryVersion,
		install: config.Installer, ingress: config.Ingress, retain: config.RetainRuntime,
		drainTimeout: config.DrainTimeout, load: load,
		maxResidentTenants:      config.MaxResidentTenants,
		activationWorkAdmission: config.ActivationWorkAdmission,
		operationGate:           make(chan struct{}, 1), maxWaiters: config.MaxActivationWaiters, operationWait: config.ActivationWait,
		managementRetires: make(map[string]*tenantRetirement),
	}, nil
}

func (c *TenantRuntimeCoordinator) acquireOperation(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case c.operationGate <- struct{}{}:
		return func() { <-c.operationGate }, nil
	default:
	}
	c.waitersMu.Lock()
	if c.waiters >= c.maxWaiters {
		c.waitersMu.Unlock()
		c.rejected.Add(1)
		return nil, tenantruntime.ErrActivationCapacity
	}
	c.waiters++
	c.waitersMu.Unlock()
	started := time.Now()
	defer func() {
		c.waitCount.Add(1)
		c.waitNanos.Add(uint64(time.Since(started).Nanoseconds()))
		c.waitersMu.Lock()
		c.waiters--
		c.waitersMu.Unlock()
	}()
	timer := time.NewTimer(c.operationWait)
	defer timer.Stop()
	select {
	case c.operationGate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-c.operationGate
			c.rejected.Add(1)
			return nil, err
		}
		return func() { <-c.operationGate }, nil
	case <-ctx.Done():
		c.rejected.Add(1)
		return nil, ctx.Err()
	case <-timer.C:
		c.rejected.Add(1)
		return nil, tenantruntime.ErrActivationCapacity
	}
}

type ActivationAdmissionSnapshot struct {
	Waiters      uint16
	MaxWaiters   uint16
	WaitCount    uint64
	WaitDuration time.Duration
	Rejected     uint64
}

func (c *TenantRuntimeCoordinator) AdmissionSnapshot() ActivationAdmissionSnapshot {
	if c == nil {
		return ActivationAdmissionSnapshot{}
	}
	c.waitersMu.Lock()
	waiters := c.waiters
	c.waitersMu.Unlock()
	return ActivationAdmissionSnapshot{Waiters: waiters, MaxWaiters: c.maxWaiters, WaitCount: c.waitCount.Load(), WaitDuration: time.Duration(c.waitNanos.Load()), Rejected: c.rejected.Load()}
}

func (c *TenantRuntimeCoordinator) Install(info site.DBSiteInfo, runtime *knet.LoadedSite) error {
	if c == nil || c.install == nil {
		return errors.New("tenant runtime coordinator is unavailable")
	}
	return c.install(info, runtime)
}

func (c *TenantRuntimeCoordinator) Add(ctx context.Context, info site.DBSiteInfo, directoryRevision uint64) error {
	if c == nil {
		return errors.New("tenant runtime coordinator is unavailable")
	}
	release, err := c.acquireOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	if c.tenants.SnapshotByID(info.SiteID) != nil {
		return nil
	}
	return c.addLocked(ctx, info, directoryRevision)
}

func (c *TenantRuntimeCoordinator) ResidentCapacity() int {
	if c == nil {
		return 0
	}
	return int(c.maxResidentTenants)
}

func (c *TenantRuntimeCoordinator) addLocked(ctx context.Context, info site.DBSiteInfo, directoryRevision uint64) error {
	if c.maxResidentTenants > 0 {
		active, retiring := c.tenants.ResourceCounts()
		if uint32(active+retiring) >= c.maxResidentTenants {
			retired, err := c.tenants.EvictLeastRecentlyUsedIdle(ctx)
			if err != nil {
				return errors.Join(tenantruntime.ErrResidentCapacity, err)
			}
			if err := retired.Close(ctx); err != nil {
				return errors.Join(tenantruntime.ErrResidentCapacity, fmt.Errorf("close evicted tenant runtime %q: %w", retired.Name, err))
			}
			c.tenants.FinishRetirement(retired)
		}
	}
	releaseActivationWork, err := c.acquireActivationWork(ctx, info)
	if err != nil {
		return fmt.Errorf("admit tenant activation: %w", err)
	}
	runtime, err := c.load(ctx, info, c.common, nil, c.version)
	releaseActivationWork()
	if err != nil {
		return fmt.Errorf("load tenant runtime: %w", err)
	}
	if runtime == nil {
		return errors.New("tenant runtime loader returned no runtime")
	}
	runtime.DirectoryRevision = directoryRevision
	if err := c.install(info, runtime); err != nil {
		return c.discard(ctx, runtime, false, fmt.Errorf("install tenant runtime services: %w", err))
	}
	if err := c.tenants.AddSite(runtime); err != nil {
		return c.discard(ctx, runtime, false, asReloadConflict(fmt.Errorf("publish tenant runtime: %w", err)))
	}
	if c.ingress != nil {
		c.ingress.MarkLocalReady(info.SiteID, true)
	}
	return nil
}

func (c *TenantRuntimeCoordinator) Replace(ctx context.Context, info site.DBSiteInfo, directoryRevision uint64) error {
	if c == nil {
		return errors.New("tenant runtime coordinator is unavailable")
	}
	release, err := c.acquireOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	return c.replaceLocked(ctx, info, directoryRevision)
}

func (c *TenantRuntimeCoordinator) replaceLocked(ctx context.Context, info site.DBSiteInfo, directoryRevision uint64) error {
	previous, previousHandle, err := c.tenants.PinExistingByID(ctx, info.SiteID)
	if err != nil && !errors.Is(err, tenantruntime.ErrTenantNotActive) {
		return fmt.Errorf("pin previous tenant generation: %w", err)
	}
	if previousHandle != nil {
		defer previousHandle.Release()
	}
	releaseActivationWork, err := c.acquireActivationWork(ctx, info)
	if err != nil {
		return fmt.Errorf("admit tenant replacement: %w", err)
	}
	next, err := c.load(ctx, info, c.common, previous, c.version)
	releaseActivationWork()
	if err != nil {
		return fmt.Errorf("load refreshed tenant runtime: %w", err)
	}
	if next == nil {
		return errors.New("tenant runtime loader returned no replacement")
	}
	if directoryRevision < next.DirectoryRevision {
		directoryRevision = next.DirectoryRevision
	}
	next.DirectoryRevision = directoryRevision
	sharesStableResources := previous != nil && next.DB != nil && next.DB == previous.DB
	if err := c.install(info, next); err != nil {
		return c.discard(ctx, next, sharesStableResources, fmt.Errorf("install refreshed tenant services: %w", err))
	}
	retired, err := c.tenants.ReplaceSiteContext(ctx, next)
	if err != nil {
		return c.discard(ctx, next, sharesStableResources, fmt.Errorf("publish refreshed tenant runtime: %w", err))
	}
	if retired != nil {
		TransferStableTenantResources(retired, next)
		c.startRetiredDrain(retired)
	}
	if c.ingress != nil {
		c.ingress.MarkLocalReady(info.SiteID, true)
	}
	return nil
}

func (c *TenantRuntimeCoordinator) acquireActivationWork(ctx context.Context, info site.DBSiteInfo) (func(), error) {
	if c.activationWorkAdmission == nil {
		return func() {}, nil
	}
	return c.activationWorkAdmission(ctx, info)
}

// ActivateFromDirectory resolves one credential-free descriptor first, then
// fetches that tenant's data-plane credentials and publishes at most one
// generation even when many first requests race on a cold alias.
func (c *TenantRuntimeCoordinator) ActivateFromDirectory(ctx context.Context, siteID string) error {
	if c == nil || c.directory == nil || c.tenants == nil {
		return errors.New("tenant lazy activation requires the Engine directory and runtime manager")
	}
	release, err := c.acquireOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	descriptor, err := tenantDescriptorByID(ctx, c.directory, siteID)
	if err != nil {
		return fmt.Errorf("read tenant descriptor for activation: %w", err)
	}
	if descriptor.Status != "active" && descriptor.Status != "degraded" {
		return fmt.Errorf("tenant is not accepting traffic")
	}
	info, err := tenantInfoByID(ctx, c.directory, siteID)
	if err != nil {
		return fmt.Errorf("resolve tenant credentials for activation: %w", err)
	}
	if info.Status != "active" && info.Status != "degraded" {
		return fmt.Errorf("tenant is not accepting traffic")
	}
	directoryRevision, err := tenantDirectoryRevision(ctx, c.directory)
	if err != nil {
		return fmt.Errorf("read directory revision for tenant activation: %w", err)
	}
	if snapshot := c.tenants.SnapshotByID(siteID); snapshot != nil {
		if snapshot.ConfigRevision >= info.ConfigRevision {
			if snapshot.ConfigRevision == info.ConfigRevision {
				descriptor.DirectoryRevision = directoryRevision
				return c.tenants.ApplySiteDescriptor(descriptor)
			}
			if directoryRevision > snapshot.Activation.DirectoryRevision {
				return c.tenants.AdvanceDirectoryRevision(siteID, directoryRevision)
			}
			return nil
		}
		return c.replaceLocked(ctx, info, directoryRevision)
	}
	return c.addLocked(ctx, info, directoryRevision)
}

type contextualTenantDirectory interface {
	GetDescriptorByIDContext(context.Context, string) (site.SiteDescriptor, error)
	GetByIDContext(context.Context, string) (site.DBSiteInfo, error)
	CurrentDirectoryRevisionContext(context.Context) (uint64, error)
}

func tenantDescriptorByID(ctx context.Context, directory TenantRuntimeDirectory, siteID string) (site.SiteDescriptor, error) {
	if contextual, ok := directory.(contextualTenantDirectory); ok {
		return contextual.GetDescriptorByIDContext(ctx, siteID)
	}
	return directory.GetDescriptorByID(siteID)
}

func tenantInfoByID(ctx context.Context, directory TenantRuntimeDirectory, siteID string) (site.DBSiteInfo, error) {
	if contextual, ok := directory.(contextualTenantDirectory); ok {
		return contextual.GetByIDContext(ctx, siteID)
	}
	return directory.GetByID(siteID)
}

func tenantDirectoryRevision(ctx context.Context, directory TenantRuntimeDirectory) (uint64, error) {
	if contextual, ok := directory.(contextualTenantDirectory); ok {
		return contextual.CurrentDirectoryRevisionContext(ctx)
	}
	return directory.CurrentDirectoryRevision()
}

func (c *TenantRuntimeCoordinator) Retire(ctx context.Context, siteID string) error {
	if c == nil {
		return errors.New("tenant runtime coordinator is unavailable")
	}
	release, err := c.acquireOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	retired, err := c.tenants.RemoveSiteByIDContext(ctx, siteID)
	if err != nil {
		return fmt.Errorf("retiring tenant generation capacity exhausted: %w", err)
	}
	if retired == nil {
		return c.waitManagementRetirement(ctx, siteID)
	}
	if c.ingress != nil {
		c.ingress.MarkLocalReady(siteID, false)
	}
	completion := c.startManagementRetirement(siteID, retired)
	select {
	case <-completion.done:
		return completion.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *TenantRuntimeCoordinator) waitManagementRetirement(ctx context.Context, siteID string) error {
	c.managementRetireMu.Lock()
	completion := c.managementRetires[siteID]
	c.managementRetireMu.Unlock()
	if completion == nil {
		return nil
	}
	select {
	case <-completion.done:
		return completion.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *TenantRuntimeCoordinator) startManagementRetirement(siteID string, runtime *knet.LoadedSite) *tenantRetirement {
	c.managementRetireMu.Lock()
	if completion := c.managementRetires[siteID]; completion != nil {
		c.managementRetireMu.Unlock()
		return completion
	}
	completion := &tenantRetirement{done: make(chan struct{})}
	c.managementRetires[siteID] = completion
	c.managementRetireMu.Unlock()
	c.retiredDrains.Add(1)
	go func() {
		defer c.retiredDrains.Done()
		if err := DrainRetiredTenantRuntime(runtime, c.drainTimeout); err != nil {
			c.managementRetireMu.Lock()
			completion.err = err
			close(completion.done)
			c.managementRetireMu.Unlock()
			return
		}
		c.tenants.FinishRetirement(runtime)
		c.managementRetireMu.Lock()
		if c.managementRetires[siteID] == completion {
			delete(c.managementRetires, siteID)
		}
		close(completion.done)
		c.managementRetireMu.Unlock()
	}()
	return completion
}

func (c *TenantRuntimeCoordinator) Retain(runtime *knet.LoadedSite) {
	if c != nil && c.retain != nil && runtime != nil {
		c.retain(runtime)
	}
}

func (c *TenantRuntimeCoordinator) discard(ctx context.Context, runtime *knet.LoadedSite, sharesStableResources bool, cause error) error {
	if runtime == nil {
		return cause
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if sharesStableResources {
		runtime.DB = nil
		runtime.SessionManager = nil
	}
	closeErr := runtime.Close(ctx)
	if closeErr != nil {
		c.Retain(runtime)
	}
	return errors.Join(cause, closeErr)
}

func (c *TenantRuntimeCoordinator) startRetiredDrain(runtime *knet.LoadedSite) {
	c.retiredDrains.Add(1)
	go func() {
		defer c.retiredDrains.Done()
		if err := DrainRetiredTenantRuntime(runtime, c.drainTimeout); err != nil {
			return
		}
		c.tenants.FinishRetirement(runtime)
	}()
}

func (c *TenantRuntimeCoordinator) WaitRetired(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return waitForGroup(ctx, "retired tenant runtime drains", &c.retiredDrains)
}
