package graph

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/asenawritescode/kora/contract"
)

// Memory is an in-memory, concurrency-safe ResourceRegistry (GRAPH-001). It
// enforces namespace isolation and monotonic per-(namespace, name) versioning.
// It is the reference implementation against which the SQL-backed registry
// (backed by _kora_resource) is contract-tested.
type Memory struct {
	mu     sync.RWMutex
	byRef  map[refKey]contract.ResourceDescriptor
	latest map[nsName]int
}

type refKey struct {
	namespace string
	name      string
	version   int
}

type nsName struct {
	namespace string
	name      string
}

// NewMemory returns an empty in-memory registry.
func NewMemory() *Memory {
	return &Memory{
		byRef:  make(map[refKey]contract.ResourceDescriptor),
		latest: make(map[nsName]int),
	}
}

// Register stores d under its namespaced identity. Re-registering an existing
// (namespace, name) bumps the version; re-registering the exact same
// (namespace, name, version) with an identical content hash is idempotent and
// returns the existing ref unchanged. A version collision with a different
// hash is a typed conflict.
func (m *Memory) Register(ctx context.Context, d contract.ResourceDescriptor) (contract.ResourceRef, error) {
	if d.Ref.Namespace == "" {
		return contract.ResourceRef{}, contract.ErrResourceNamespaceRequired
	}
	if d.Ref.Name == "" {
		return contract.ResourceRef{}, contract.ErrResourceNameRequired
	}
	if !knownKind(d.Kind) {
		return contract.ResourceRef{}, fmt.Errorf("%w: %q", contract.ErrResourceKindUnknown, d.Kind)
	}

	hash := DescriptorHash(d)
	d.Hash = hash
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	key := refKey{namespace: d.Ref.Namespace, name: d.Ref.Name, version: d.Ref.Version}

	// Exact (namespace, name, version) already present.
	if existing, ok := m.byRef[key]; ok {
		if existing.Hash == hash {
			return existing.Ref, nil // idempotent re-register
		}
		return contract.ResourceRef{}, contract.ErrResourceVersionConflict
	}

	// Determine the version to assign: honor an explicit positive version, else
	// bump the latest for this (namespace, name).
	nn := nsName{namespace: d.Ref.Namespace, name: d.Ref.Name}
	if d.Ref.Version <= 0 {
		d.Ref.Version = m.latest[nn] + 1
	}
	if d.Ref.Version <= m.latest[nn] {
		// A stale explicit version collides with an already-registered newer one.
		if _, ok := m.byRef[refKey{namespace: d.Ref.Namespace, name: d.Ref.Name, version: d.Ref.Version}]; ok {
			return contract.ResourceRef{}, contract.ErrResourceVersionConflict
		}
	}

	key = refKey{namespace: d.Ref.Namespace, name: d.Ref.Name, version: d.Ref.Version}
	m.byRef[key] = d
	if d.Ref.Version > m.latest[nn] {
		m.latest[nn] = d.Ref.Version
	}
	return d.Ref, nil
}

func knownKind(k contract.ResourceKind) bool {
	switch k {
	case contract.ResourceKindDoctype, contract.ResourceKindCollection, contract.ResourceKindCommand, contract.ResourceKindQuery,
		contract.ResourceKindEntity, contract.ResourceKindRelation, contract.ResourceKindComputation, contract.ResourceKindRule,
		contract.ResourceKindPolicy, contract.ResourceKindAction, contract.ResourceKindWorkflow, contract.ResourceKindCapability,
		contract.ResourceKindSkill, contract.ResourceKindRole, contract.ResourceKindAgent, contract.ResourceKindView,
		contract.ResourceKindEvent, contract.ResourceKindIntegration, contract.ResourceKindPlugin, contract.ResourceKindPackage,
		contract.ResourceKindPage, contract.ResourceKindComponent:
		return true
	default:
		return false
	}
}

// Resolve returns the descriptor for ref. A Version of 0 resolves the latest
// registered version for that (namespace, name); otherwise the version must
// match exactly.
func (m *Memory) Resolve(ref contract.ResourceRef) (contract.ResourceDescriptor, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	version := ref.Version
	if version <= 0 {
		version = m.latest[nsName{namespace: ref.Namespace, name: ref.Name}]
	}
	d, ok := m.byRef[refKey{namespace: ref.Namespace, name: ref.Name, version: version}]
	if !ok {
		return contract.ResourceDescriptor{}, contract.ErrResourceNotFound
	}
	return d, nil
}

// List returns all descriptors in namespace, optionally filtered by kind (an
// empty kind matches all kinds). Results are ordered by (name, version).
func (m *Memory) List(namespace string, kind contract.ResourceKind) ([]contract.ResourceDescriptor, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]contract.ResourceDescriptor, 0)
	for k, d := range m.byRef {
		if k.namespace != namespace {
			continue
		}
		if kind != "" && d.Kind != kind {
			continue
		}
		out = append(out, d)
	}
	sortDescriptors(out)
	return out, nil
}

func sortDescriptors(ds []contract.ResourceDescriptor) {
	for i := 1; i < len(ds); i++ {
		for j := i; j > 0 && less(ds[j], ds[j-1]); j-- {
			ds[j], ds[j-1] = ds[j-1], ds[j]
		}
	}
}

func less(a, b contract.ResourceDescriptor) bool {
	if a.Ref.Name != b.Ref.Name {
		return a.Ref.Name < b.Ref.Name
	}
	return a.Ref.Version < b.Ref.Version
}
