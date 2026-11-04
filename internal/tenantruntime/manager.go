package tenantruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/asenawritescode/kora/contract"
)

var (
	ErrManagerClosed        = errors.New("tenant runtime manager is closed")
	ErrTenantNotActive      = errors.New("tenant runtime is not active")
	ErrActivationMismatch   = errors.New("tenant runtime does not match activation")
	ErrTenantStillAccepting = errors.New("tenant runtime must stop accepting traffic before drain")
	ErrResidentCapacity     = errors.New("resident tenant capacity exhausted")
	ErrActivationCapacity   = errors.New("tenant activation admission capacity exhausted")
	ErrNoIdleTenantRuntime  = errors.New("no idle tenant runtime is available for eviction")
)

type directorySnapshot struct {
	all            []*Runtime
	byName         map[string]*Runtime
	byID           map[string]*Runtime
	byHost         map[string]*Runtime
	ambiguousIDs   map[string]bool
	ambiguousHosts map[string]bool
	defaultRuntime *Runtime
}

// Manager is the sole publisher of active tenant runtime snapshots. Readers
// acquire against a coherent index and pin that exact generation before a
// replacement can retire it.
type Manager struct {
	mu              sync.RWMutex
	closed          bool
	active          atomic.Pointer[directorySnapshot]
	retiring        *Retirements
	retired         map[uint64]*Runtime
	reserved        map[uint64]bool
	retirementTasks map[uint64]chan struct{}
	maxResident     uint32
}

func NewManager(initial []*Runtime, maxRetiring uint16, maxResident ...uint32) (*Manager, error) {
	retiring, err := NewRetirements(maxRetiring)
	if err != nil {
		return nil, err
	}
	var residentLimit uint32
	if len(maxResident) > 0 {
		residentLimit = maxResident[0]
	}
	if residentLimit > 0 && uint32(len(initial)) > residentLimit {
		return nil, fmt.Errorf("initial resident tenant count %d exceeds configured limit %d: %w", len(initial), residentLimit, ErrResidentCapacity)
	}
	m := &Manager{retiring: retiring, retired: make(map[uint64]*Runtime), reserved: make(map[uint64]bool), retirementTasks: make(map[uint64]chan struct{}), maxResident: residentLimit}
	for _, runtime := range initial {
		if runtime != nil && runtime.Status == "" {
			runtime.Status = "active"
		}
	}
	m.active.Store(buildDirectorySnapshot(initial))
	return m, nil
}

func buildDirectorySnapshot(runtimes []*Runtime) *directorySnapshot {
	s := &directorySnapshot{
		all: append([]*Runtime(nil), runtimes...), byName: make(map[string]*Runtime),
		byID: make(map[string]*Runtime), byHost: make(map[string]*Runtime),
		ambiguousIDs: make(map[string]bool), ambiguousHosts: make(map[string]bool),
	}
	for _, runtime := range s.all {
		if runtime == nil {
			continue
		}
		if runtime.SiteID != "" {
			if previous := s.byID[runtime.SiteID]; previous != nil && previous != runtime {
				delete(s.byID, runtime.SiteID)
				s.ambiguousIDs[runtime.SiteID] = true
			} else if !s.ambiguousIDs[runtime.SiteID] {
				s.byID[runtime.SiteID] = runtime
			}
		}
		if previous := s.byName[runtime.Name]; previous != nil && previous != runtime {
			delete(s.byName, runtime.Name)
		} else {
			s.byName[runtime.Name] = runtime
		}
		for _, alias := range aliases(runtime.Config) {
			alias = normalizeHost(alias)
			if alias == "" {
				continue
			}
			if previous := s.byHost[alias]; previous != nil && previous != runtime {
				delete(s.byHost, alias)
				s.ambiguousHosts[alias] = true
			} else if !s.ambiguousHosts[alias] {
				s.byHost[alias] = runtime
			}
		}
	}
	for _, runtime := range s.all {
		if runtime != nil && acceptsTraffic(runtime) {
			s.defaultRuntime = runtime
			break
		}
	}
	return s
}

func aliases(config SiteConfig) []string {
	all := append([]string(nil), config.Domains...)
	return append(all, config.Hostname)
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}
func acceptsTraffic(runtime *Runtime) bool {
	return runtime != nil && (runtime.Status == "" || runtime.Status == "active" || runtime.Status == "degraded")
}

func (m *Manager) current() *directorySnapshot {
	if m == nil {
		return &directorySnapshot{}
	}
	if snapshot := m.active.Load(); snapshot != nil {
		return snapshot
	}
	return &directorySnapshot{}
}

func (m *Manager) All() []*Runtime {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]*Runtime(nil), m.current().all...)
}

// AllSnapshots returns an immutable copy of the active tenant metadata.
func (m *Manager) AllSnapshots() []*Snapshot {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil
	}
	runtimes := m.current().all
	snapshots := make([]*Snapshot, 0, len(runtimes))
	for _, runtime := range runtimes {
		if runtime != nil {
			snapshots = append(snapshots, runtime.Snapshot())
		}
	}
	return snapshots
}

// SnapshotByID returns a coherent metadata snapshot without admitting work.
func (m *Manager) SnapshotByID(tenant contract.TenantID) *Snapshot {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil
	}
	s := m.current()
	if s.ambiguousIDs[string(tenant)] {
		return nil
	}
	if runtime := s.byID[string(tenant)]; runtime != nil {
		return runtime.Snapshot()
	}
	return nil
}

// SnapshotByName returns a coherent metadata snapshot without admitting work.
func (m *Manager) SnapshotByName(name string) *Snapshot {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil
	}
	if runtime := lookupName(m.current(), name); runtime != nil {
		return runtime.Snapshot()
	}
	return nil
}

func (m *Manager) ByID(id string) *Runtime {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.current()
	if s.ambiguousIDs[id] {
		return nil
	}
	return s.byID[id]
}

func (m *Manager) ByName(name string) *Runtime {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return lookupName(m.current(), name)
}

func (m *Manager) ByHost(host string) (*Runtime, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.current()
	host = normalizeHost(host)
	if s.ambiguousHosts[host] {
		return nil, true
	}
	return s.byHost[host], false
}

func lookupName(s *directorySnapshot, name string) *Runtime {
	if s.ambiguousIDs[name] {
		return nil
	}
	if runtime := s.byID[name]; runtime != nil {
		return runtime
	}
	if runtime := s.byName[name]; runtime != nil {
		return runtime
	}
	for _, runtime := range s.all {
		short := strings.TrimSuffix(runtime.Name, ".local")
		short = strings.TrimSuffix(short, ".com")
		short = strings.TrimSuffix(short, ".app")
		if short == name {
			return runtime
		}
	}
	return nil
}

// Acquire pins the currently published tenant activation by stable tenant ID.
func (m *Manager) Acquire(ctx context.Context, tenant contract.TenantID) (*Handle, error) {
	_, handle, err := m.acquireExisting(ctx, tenant, true)
	return handle, err
}

// PinExisting pins the active generation by stable tenant ID even when its
// status currently fences new traffic. It is reserved for control-plane
// replacement/retirement code that must safely inspect or transfer resources.
func (m *Manager) PinExisting(ctx context.Context, tenant contract.TenantID) (*Runtime, *Handle, error) {
	return m.acquireExisting(ctx, tenant, false)
}

func (m *Manager) acquireExisting(ctx context.Context, tenant contract.TenantID, requireTraffic bool) (*Runtime, *Handle, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
	}
	if m == nil {
		return nil, nil, ErrTenantNotActive
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, nil, ErrManagerClosed
	}
	s := m.current()
	if s.ambiguousIDs[string(tenant)] {
		return nil, nil, fmt.Errorf("tenant id %q is ambiguous", tenant)
	}
	runtime := s.byID[string(tenant)]
	if runtime == nil {
		return nil, nil, ErrTenantNotActive
	}
	if requireTraffic && !acceptsTraffic(runtime) {
		return nil, nil, ErrTenantNotActive
	}
	handle, ok := runtime.AcquireRuntime()
	if !ok {
		return nil, nil, ErrTenantNotActive
	}
	return runtime, handle, nil
}

func (m *Manager) AcquireByName(ctx context.Context, name string) (*Runtime, *Handle, bool) {
	if m == nil {
		return nil, nil, false
	}
	if ctx != nil && ctx.Err() != nil {
		return nil, nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, nil, false
	}
	runtime := lookupName(m.current(), name)
	if runtime == nil {
		return nil, nil, false
	}
	if !acceptsTraffic(runtime) {
		return runtime, nil, false
	}
	handle, ok := runtime.AcquireRuntime()
	if !ok {
		return runtime, nil, false
	}
	return runtime, handle, true
}

func (m *Manager) AcquireByHost(ctx context.Context, host string, fallback bool) (*Runtime, *Handle, bool, bool) {
	if m == nil {
		return nil, nil, false, false
	}
	if ctx != nil && ctx.Err() != nil {
		return nil, nil, false, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, nil, false, false
	}
	s := m.current()
	host = normalizeHost(host)
	if s.ambiguousHosts[host] {
		return nil, nil, true, false
	}
	runtime := s.byHost[host]
	if runtime == nil && fallback && (net.ParseIP(host) != nil || host == "localhost" || host == "127.0.0.1") {
		runtime = s.defaultRuntime
	}
	if runtime == nil {
		return nil, nil, false, false
	}
	if !acceptsTraffic(runtime) {
		return runtime, nil, false, false
	}
	handle, ok := runtime.AcquireRuntime()
	return runtime, handle, false, ok
}

// Activate atomically publishes a fully prepared runtime. Replacements may
// reuse a config revision, but neither config nor directory revisions regress.
func (m *Manager) Activate(ctx context.Context, activation Activation, candidate *Runtime) (*Runtime, error) {
	if m == nil || candidate == nil {
		return nil, errors.New("tenant runtime and manager are required")
	}
	configRevision, err := parseRevision(activation.ConfigRevision)
	if err != nil || (string(activation.Tenant) == "" && candidate.SiteID != "") || (string(activation.Tenant) != "" && (candidate.SiteID != string(activation.Tenant) || candidate.ConfigRevision != configRevision || candidate.DirectoryRevision != activation.DirectoryRevision || string(activation.ResourceRevision) != candidate.DatabaseIdentity)) {
		return nil, ErrActivationMismatch
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		m.mu.RLock()
		if m.closed {
			m.mu.RUnlock()
			return nil, ErrManagerClosed
		}
		old := findExisting(m.current(), candidate)
		m.mu.RUnlock()
		reserved := false
		if old != nil {
			if err := m.retiring.Acquire(ctx); err != nil {
				return nil, err
			}
			reserved = true
		}
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			if reserved {
				m.retiring.Release()
			}
			return nil, ErrManagerClosed
		}
		s := m.current()
		if findExisting(s, candidate) != old {
			m.mu.Unlock()
			if reserved {
				m.retiring.Release()
			}
			continue
		}
		if old != nil && (candidate.ConfigRevision < old.ConfigRevision || candidate.DirectoryRevision < old.DirectoryRevision) {
			m.mu.Unlock()
			if reserved {
				m.retiring.Release()
			}
			return nil, fmt.Errorf("stale tenant activation for %q", candidate.SiteID)
		}
		if err := validateCandidate(s, candidate, old); err != nil {
			m.mu.Unlock()
			if reserved {
				m.retiring.Release()
			}
			return nil, err
		}
		if old == nil && m.maxResident > 0 && uint32(len(s.all)) >= m.maxResident {
			m.mu.Unlock()
			return nil, ErrResidentCapacity
		}
		if candidate.Status == "" {
			candidate.Status = "active"
		}
		candidate.EnsureRuntimeGeneration()
		all := make([]*Runtime, 0, len(s.all)+1)
		for _, runtime := range s.all {
			if runtime != old {
				all = append(all, runtime)
			}
		}
		all = append(all, candidate)
		if old != nil {
			old.BeginDrain()
			generation := old.EnsureRuntimeGeneration()
			m.retired[generation] = old
			m.reserved[generation] = reserved
		}
		m.active.Store(buildDirectorySnapshot(all))
		m.mu.Unlock()
		return old, nil
	}
}

func findExisting(s *directorySnapshot, candidate *Runtime) *Runtime {
	if candidate.SiteID != "" {
		if s.ambiguousIDs[candidate.SiteID] {
			return nil
		}
		if runtime := s.byID[candidate.SiteID]; runtime != nil {
			return runtime
		}
	}
	return lookupName(s, candidate.Name)
}

func activationFor(runtime *Runtime) Activation {
	return Activation{
		Tenant:            contract.TenantID(runtime.SiteID),
		ConfigRevision:    contract.Revision(strconv.FormatUint(runtime.ConfigRevision, 10)),
		DirectoryRevision: runtime.DirectoryRevision,
		ResourceRevision:  contract.Revision(runtime.DatabaseIdentity),
	}
}

func (m *Manager) Add(ctx context.Context, runtime *Runtime) error {
	if runtime == nil {
		return errors.New("tenant runtime is required")
	}
	if m == nil {
		return ErrManagerClosed
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	m.mu.RLock()
	current, closed := findExisting(m.current(), runtime), m.closed
	m.mu.RUnlock()
	if closed {
		return ErrManagerClosed
	}
	if current == runtime {
		return nil
	}
	old, err := m.Activate(ctx, activationFor(runtime), runtime)
	if err != nil {
		return err
	}
	if old != nil {
		m.startRetirement(old)
	}
	return nil
}

func (m *Manager) startRetirement(runtime *Runtime) {
	if m == nil || runtime == nil {
		return
	}
	generation := runtime.EnsureRuntimeGeneration()
	m.mu.Lock()
	if m.closed || m.retirementTasks[generation] != nil {
		m.mu.Unlock()
		return
	}
	done := make(chan struct{})
	m.retirementTasks[generation] = done
	m.mu.Unlock()
	go func() {
		if err := runtime.Close(context.Background()); err != nil {
			slog.Error("tenant runtime retirement cleanup failed", "tenant", runtime.SiteID, "error", err)
		} else {
			m.FinishRetirement(runtime)
		}
		m.mu.Lock()
		delete(m.retirementTasks, generation)
		close(done)
		m.mu.Unlock()
	}()
}

func parseRevision(revision contract.Revision) (uint64, error) {
	parsed, err := strconv.ParseUint(string(revision), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid config revision %q: %w", revision, err)
	}
	return parsed, nil
}

func validateCandidate(s *directorySnapshot, candidate, replaced *Runtime) error {
	if replaced != nil && replaced.SiteID != "" && candidate.SiteID != "" && replaced.SiteID != candidate.SiteID {
		return fmt.Errorf("tenant identity cannot change during runtime replacement")
	}
	for _, alias := range aliases(candidate.Config) {
		alias = normalizeHost(alias)
		if alias == "" {
			continue
		}
		for _, owner := range s.all {
			if owner == replaced {
				continue
			}
			for _, existing := range aliases(owner.Config) {
				if normalizeHost(existing) == alias {
					return fmt.Errorf("site alias %q is already assigned to %q", alias, owner.Name)
				}
			}
		}
	}
	for _, owner := range s.all {
		if candidate.SiteID != "" && owner != replaced && owner.SiteID == candidate.SiteID {
			return fmt.Errorf("canonical tenant id %q is already assigned to %q", candidate.SiteID, owner.Name)
		}
	}
	return nil
}

func (m *Manager) Retire(ctx context.Context, tenant contract.TenantID) (*Runtime, error) {
	if m == nil {
		return nil, ErrTenantNotActive
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return nil, ErrManagerClosed
	}
	current := m.current()
	if current.byID[string(tenant)] == nil || current.ambiguousIDs[string(tenant)] {
		m.mu.RUnlock()
		return nil, nil
	}
	m.mu.RUnlock()
	if err := m.retiring.Acquire(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.retiring.Release()
		return nil, ErrManagerClosed
	}
	s := m.current()
	runtime := s.byID[string(tenant)]
	if runtime == nil || s.ambiguousIDs[string(tenant)] {
		m.mu.Unlock()
		m.retiring.Release()
		return nil, nil
	}
	runtime.BeginDrain()
	all := make([]*Runtime, 0, len(s.all)-1)
	for _, current := range s.all {
		if current != runtime {
			all = append(all, current)
		}
	}
	gen := runtime.EnsureRuntimeGeneration()
	m.retired[gen] = runtime
	m.reserved[gen] = true
	m.active.Store(buildDirectorySnapshot(all))
	m.mu.Unlock()
	return runtime, nil
}

// EvictLeastRecentlyUsedIdle removes the least recently used runtime only if
// it has no admitted handles. The caller owns drain and resource close; the
// manager retains the generation and its resident slot until FinishRetirement.
func (m *Manager) EvictLeastRecentlyUsedIdle(ctx context.Context) (*Runtime, error) {
	if m == nil {
		return nil, ErrManagerClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.retiring.Acquire(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.retiring.Release()
		return nil, ErrManagerClosed
	}
	snapshot := m.current()
	candidates := append([]*Runtime(nil), snapshot.all...)
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i].lastUsedUnixNano(), candidates[j].lastUsedUnixNano()
		if left == right {
			return candidates[i].SiteID < candidates[j].SiteID
		}
		return left < right
	})
	var retired *Runtime
	for _, candidate := range candidates {
		if !candidate.retireIfIdle() {
			continue
		}
		retired = candidate
		break
	}
	if retired == nil {
		m.mu.Unlock()
		m.retiring.Release()
		return nil, ErrNoIdleTenantRuntime
	}
	all := make([]*Runtime, 0, len(snapshot.all)-1)
	for _, runtime := range snapshot.all {
		if runtime != retired {
			all = append(all, runtime)
		}
	}
	generation := retired.EnsureRuntimeGeneration()
	m.retired[generation] = retired
	m.reserved[generation] = true
	m.active.Store(buildDirectorySnapshot(all))
	m.mu.Unlock()
	return retired, nil
}

// FinishRetirement releases the manager's retirement capacity after the caller
// has waited for request drain and closed the retired runtime's resources.
func (m *Manager) FinishRetirement(runtime *Runtime) {
	if m == nil || runtime == nil {
		return
	}
	m.mu.Lock()
	generation := runtime.EnsureRuntimeGeneration()
	_, exists := m.retired[generation]
	reserved := m.reserved[generation]
	delete(m.retired, generation)
	delete(m.reserved, generation)
	m.mu.Unlock()
	if exists && reserved {
		m.retiring.Release()
	}
}

func (m *Manager) IsAmbiguousHost(host string) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current().ambiguousHosts[normalizeHost(host)]
}

func (m *Manager) UpdateConfig(name string, config SiteConfig) *Runtime {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	s := m.current()
	runtime := lookupName(s, name)
	if runtime == nil {
		return nil
	}
	candidate := &Runtime{SiteID: runtime.SiteID, Name: runtime.Name, Config: config}
	if validateCandidate(s, candidate, runtime) != nil {
		return nil
	}
	runtime.Config = config
	m.active.Store(buildDirectorySnapshot(s.all))
	return runtime
}

func (m *Manager) SetStatus(tenant contract.TenantID, status string, revision uint64) error {
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "provisioning", "active", "degraded", "suspended", "deleting", "deleted":
	default:
		return fmt.Errorf("unsupported site status %q", status)
	}
	if m == nil {
		return ErrTenantNotActive
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrManagerClosed
	}
	s := m.current()
	runtime := s.byID[string(tenant)]
	if runtime == nil || s.ambiguousIDs[string(tenant)] {
		return fmt.Errorf("site id %q is not loaded", tenant)
	}
	if revision < runtime.ConfigRevision || (revision == runtime.ConfigRevision && status != runtime.Status) {
		return fmt.Errorf("stale or conflicting site revision %d; current revision is %d", revision, runtime.ConfigRevision)
	}
	runtime.Status, runtime.ConfigRevision = status, revision
	m.active.Store(buildDirectorySnapshot(s.all))
	return nil
}

func (m *Manager) AdvanceDirectoryRevision(tenant contract.TenantID, revision uint64) error {
	if revision == 0 {
		return errors.New("directory revision must be positive")
	}
	if m == nil {
		return ErrTenantNotActive
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrManagerClosed
	}
	s := m.current()
	runtime := s.byID[string(tenant)]
	if runtime == nil || s.ambiguousIDs[string(tenant)] {
		return fmt.Errorf("site id %q is not loaded", tenant)
	}
	if revision < runtime.DirectoryRevision {
		return fmt.Errorf("stale directory revision %d; current revision is %d", revision, runtime.DirectoryRevision)
	}
	runtime.DirectoryRevision = revision
	m.active.Store(buildDirectorySnapshot(s.all))
	return nil
}

func (m *Manager) WaitForIdle(ctx context.Context, tenant contract.TenantID) error {
	if m == nil {
		return ErrTenantNotActive
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return ErrManagerClosed
	}
	s := m.current()
	runtime := s.byID[string(tenant)]
	if runtime == nil || s.ambiguousIDs[string(tenant)] {
		m.mu.RUnlock()
		return ErrTenantNotActive
	}
	if acceptsTraffic(runtime) {
		m.mu.RUnlock()
		return ErrTenantStillAccepting
	}
	err := runtime.WaitForIdle(ctx)
	m.mu.RUnlock()
	return err
}

// RetireUnavailable removes a traffic-fenced tenant only after its requests
// are idle. It needs no retirement slot because no request can be admitted and
// it verifies the drain while holding the publication lock.
func (m *Manager) RetireUnavailable(ctx context.Context, tenant contract.TenantID) (*Runtime, error) {
	if m == nil {
		return nil, ErrTenantNotActive
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrManagerClosed
	}
	s := m.current()
	runtime := s.byID[string(tenant)]
	if runtime == nil || s.ambiguousIDs[string(tenant)] {
		m.mu.Unlock()
		return nil, ErrTenantNotActive
	}
	if acceptsTraffic(runtime) {
		m.mu.Unlock()
		return nil, ErrTenantStillAccepting
	}
	if err := runtime.WaitForIdle(ctx); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	runtime.BeginDrain()
	all := make([]*Runtime, 0, len(s.all)-1)
	for _, current := range s.all {
		if current != runtime {
			all = append(all, current)
		}
	}
	generation := runtime.EnsureRuntimeGeneration()
	m.retired[generation] = runtime
	m.reserved[generation] = false
	m.active.Store(buildDirectorySnapshot(all))
	m.mu.Unlock()
	return runtime, nil
}

func (m *Manager) DirectoryRevision(tenant contract.TenantID, revision, configRevision uint64, status string, domains []string) error {
	if m == nil {
		return ErrTenantNotActive
	}
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "provisioning", "active", "degraded", "suspended", "deleting", "deleted":
	default:
		return fmt.Errorf("unsupported site status %q", status)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrManagerClosed
	}
	s := m.current()
	runtime := s.byID[string(tenant)]
	if runtime == nil || s.ambiguousIDs[string(tenant)] {
		return fmt.Errorf("site id %q is not loaded", tenant)
	}
	if configRevision < runtime.ConfigRevision || (configRevision == runtime.ConfigRevision && status != runtime.Status) {
		return fmt.Errorf("stale or conflicting site revision %d; current revision is %d", configRevision, runtime.ConfigRevision)
	}
	if revision != 0 && revision < runtime.DirectoryRevision {
		return fmt.Errorf("stale directory revision %d; current revision is %d", revision, runtime.DirectoryRevision)
	}
	candidate := &Runtime{SiteID: runtime.SiteID, Name: runtime.Name, Config: runtime.Config}
	candidate.Config.Domains = append([]string(nil), domains...)
	if err := validateCandidate(s, candidate, runtime); err != nil {
		return err
	}
	runtime.Config.Domains = append([]string(nil), domains...)
	runtime.Status = status
	runtime.ConfigRevision = configRevision
	if revision > runtime.DirectoryRevision {
		runtime.DirectoryRevision = revision
	}
	m.active.Store(buildDirectorySnapshot(s.all))
	return nil
}

func (m *Manager) ValidateAliases() error {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.current()
	owners := make(map[string]string)
	ids := make(map[string]string)
	for _, runtime := range s.all {
		if runtime.SiteID != "" {
			if owner, exists := ids[runtime.SiteID]; exists && owner != runtime.Name {
				return fmt.Errorf("canonical site id %q is assigned to both %q and %q", runtime.SiteID, owner, runtime.Name)
			}
			ids[runtime.SiteID] = runtime.Name
		}
		for _, alias := range aliases(runtime.Config) {
			alias = normalizeHost(alias)
			if alias == "" {
				continue
			}
			if owner, exists := owners[alias]; exists && owner != runtime.Name {
				return fmt.Errorf("site alias %q is assigned to both %q and %q", alias, owner, runtime.Name)
			}
			owners[alias] = runtime.Name
		}
	}
	return nil
}

func (m *Manager) Domains() []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := make(map[string]struct{})
	var result []string
	for _, runtime := range m.current().all {
		for _, alias := range aliases(runtime.Config) {
			alias = normalizeHost(alias)
			if alias == "" {
				continue
			}
			if _, ok := seen[alias]; ok {
				continue
			}
			seen[alias] = struct{}{}
			result = append(result, alias)
		}
	}
	return result
}

func (m *Manager) RemoveByName(name string) *Runtime {
	runtime, _ := m.RemoveByNameContext(context.Background(), name)
	return runtime
}

func (m *Manager) RemoveByNameContext(ctx context.Context, name string) (*Runtime, error) {
	if m == nil {
		return nil, ErrManagerClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return nil, ErrManagerClosed
	}
	if lookupName(m.current(), name) == nil {
		m.mu.RUnlock()
		return nil, nil
	}
	m.mu.RUnlock()
	if err := m.retiring.Acquire(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.retiring.Release()
		return nil, ErrManagerClosed
	}
	s := m.current()
	runtime := lookupName(s, name)
	if runtime == nil {
		m.mu.Unlock()
		m.retiring.Release()
		return nil, nil
	}
	runtime.BeginDrain()
	all := make([]*Runtime, 0, len(s.all)-1)
	for _, current := range s.all {
		if current != runtime {
			all = append(all, current)
		}
	}
	gen := runtime.EnsureRuntimeGeneration()
	m.retired[gen] = runtime
	m.reserved[gen] = true
	m.active.Store(buildDirectorySnapshot(all))
	m.mu.Unlock()
	return runtime, nil
}

func (m *Manager) RetirementCount() int {
	if m == nil {
		return 0
	}
	return m.retiring.InUse()
}

func (m *Manager) ApplyDescriptor(tenant contract.TenantID, directoryRevision, configRevision uint64, status string, inputAliases []string) error {
	if m == nil {
		return ErrTenantNotActive
	}
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "provisioning", "active", "degraded", "suspended", "deleting", "deleted":
	default:
		return fmt.Errorf("unsupported site status %q", status)
	}
	if len(inputAliases) == 0 {
		return errors.New("site descriptor aliases are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrManagerClosed
	}
	s := m.current()
	runtime := s.byID[string(tenant)]
	if runtime == nil || s.ambiguousIDs[string(tenant)] {
		return fmt.Errorf("site id %q is not loaded", tenant)
	}
	if configRevision < runtime.ConfigRevision || (configRevision == runtime.ConfigRevision && status != runtime.Status && directoryRevision <= runtime.DirectoryRevision) {
		return fmt.Errorf("stale or conflicting site revision %d; current revision is %d", configRevision, runtime.ConfigRevision)
	}
	if directoryRevision != 0 && directoryRevision < runtime.DirectoryRevision {
		return fmt.Errorf("stale directory revision %d; current revision is %d", directoryRevision, runtime.DirectoryRevision)
	}
	domains := make([]string, 0, len(inputAliases))
	seen := make(map[string]struct{}, len(inputAliases))
	for _, alias := range inputAliases {
		alias = normalizeHost(alias)
		if alias == "" || alias == normalizeHost(runtime.Name) {
			continue
		}
		if _, ok := seen[alias]; ok {
			continue
		}
		seen[alias] = struct{}{}
		for _, owner := range s.all {
			if owner == runtime {
				continue
			}
			for _, existing := range aliases(owner.Config) {
				if normalizeHost(existing) == alias {
					return fmt.Errorf("site alias %q is already assigned to %q", alias, owner.Name)
				}
			}
		}
		domains = append(domains, alias)
	}
	runtime.Config.Domains = domains
	runtime.Status = status
	runtime.ConfigRevision = configRevision
	if directoryRevision > runtime.DirectoryRevision {
		runtime.DirectoryRevision = directoryRevision
	}
	m.active.Store(buildDirectorySnapshot(s.all))
	return nil
}

// Close prevents further admission and retries closing both active and
// previously retired runtimes. Resources remain open when a drain times out.
func (m *Manager) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		for _, runtime := range m.current().all {
			runtime.BeginDrain()
			generation := runtime.EnsureRuntimeGeneration()
			m.retired[generation] = runtime
			m.reserved[generation] = false
		}
		m.active.Store(buildDirectorySnapshot(nil))
	}
	tasks := make([]chan struct{}, 0, len(m.retirementTasks))
	for _, task := range m.retirementTasks {
		tasks = append(tasks, task)
	}
	m.mu.Unlock()
	for _, task := range tasks {
		select {
		case <-task:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.mu.RLock()
	runtimes := make([]*Runtime, 0, len(m.retired))
	for _, runtime := range m.retired {
		runtimes = append(runtimes, runtime)
	}
	m.mu.RUnlock()
	var closeErr error
	for _, runtime := range runtimes {
		if err := runtime.Close(ctx); err != nil {
			closeErr = errors.Join(closeErr, err)
			continue
		}
		m.FinishRetirement(runtime)
	}
	return closeErr
}

// ResourceCounts reports bounded process diagnostics without exposing tenant
// identifiers. Active includes the currently published directory entries;
// retiring counts generations whose users or cleanup still hold resources.
func (m *Manager) ResourceCounts() (active, retiring int) {
	if m == nil {
		return 0, 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if snapshot := m.current(); snapshot != nil {
		active = len(snapshot.all)
	}
	retiring = len(m.retired)
	return active, retiring
}

// ResidentCapacity returns the configured maximum active tenant runtime count;
// zero means the compatibility manager has no explicit resident cap.
func (m *Manager) ResidentCapacity() int {
	if m == nil {
		return 0
	}
	return int(m.maxResident)
}
