// Package plugin implements provider binding via capability handles
// (PLUGIN-005): components bind to providers exclusively through PLUGIN-001
// handles, so removing a provider degrades only its dependents.
package plugin

import (
	"fmt"

	"github.com/asenawritescode/kora/component"
	"github.com/asenawritescode/kora/contract"
)

// ProviderFactory exposes one provider as a capability handle.
type ProviderFactory interface {
	Capability() contract.CapabilityName
	Open(ctx contract.OperationContext) (contract.CapabilityHandle, error)
}

// Binding is one satisfied requirement: a component's requirement resolved to a
// concrete handle.
type Binding struct {
	Component  string
	Capability contract.CapabilityName
	Handle     contract.CapabilityHandle
}

// Degradation is one unmet requirement.
type Degradation struct {
	Component  string
	Capability contract.CapabilityName
	Reason     string // "provider-absent" | "provider-unhealthy"
}

// BindingReport is the outcome of a bind pass.
type BindingReport struct {
	Bound    []Binding
	Degraded []Degradation
}

// Binder resolves component requirements against provider factories.
type Binder interface {
	Bind(ctx contract.OperationContext, manifests []component.Manifest, factories []ProviderFactory) (BindingReport, error)
}

// BindingStrategy selects strict vs degraded behavior.
type BindingStrategy int

const (
	StrategyDegraded BindingStrategy = iota // missing capability degrades, others stay live
	StrategyStrict                          // any unmet requirement aborts
)

// ErrUnsatisfiedRequirement is a typed strict-binding failure.
type ErrUnsatisfiedRequirement struct {
	Component  string
	Capability contract.CapabilityName
}

func (e *ErrUnsatisfiedRequirement) Error() string {
	return fmt.Sprintf("plugin: component %q requires unsatisfied capability %q", e.Component, e.Capability)
}

type binder struct {
	strategy BindingStrategy
}

// NewBinder returns a binder with the given strategy.
func NewBinder(strategy BindingStrategy) Binder {
	return &binder{strategy: strategy}
}

// Bind resolves each manifest's Requires against factories. In degraded mode,
// missing/unhealthy capabilities are recorded as Degradation and other
// components stay live. In strict mode, the first unmet requirement aborts with
// a typed error.
func (b *binder) Bind(ctx contract.OperationContext, manifests []component.Manifest, factories []ProviderFactory) (BindingReport, error) {
	factoryByCap := make(map[contract.CapabilityName]ProviderFactory, len(factories))
	for _, f := range factories {
		if f == nil || f.Capability() == "" {
			return BindingReport{}, fmt.Errorf("plugin: provider factory and capability are required")
		}
		if _, exists := factoryByCap[f.Capability()]; exists {
			return BindingReport{}, fmt.Errorf("plugin: multiple providers offer capability %q; explicit selection is required", f.Capability())
		}
		factoryByCap[f.Capability()] = f
	}

	var report BindingReport
	for _, m := range manifests {
		for _, req := range m.Requires {
			cap := contract.CapabilityName(req)
			f, ok := factoryByCap[cap]
			if !ok {
				if b.strategy == StrategyStrict {
					return report, &ErrUnsatisfiedRequirement{Component: m.Name, Capability: cap}
				}
				report.Degraded = append(report.Degraded, Degradation{Component: m.Name, Capability: cap, Reason: "provider-absent"})
				continue
			}
			h, err := f.Open(ctx)
			if err != nil {
				if b.strategy == StrategyStrict {
					return report, &ErrUnsatisfiedRequirement{Component: m.Name, Capability: cap}
				}
				report.Degraded = append(report.Degraded, Degradation{Component: m.Name, Capability: cap, Reason: "provider-unhealthy"})
				continue
			}
			report.Bound = append(report.Bound, Binding{Component: m.Name, Capability: cap, Handle: h})
		}
	}
	return report, nil
}

// StaticFactory is a ProviderFactory backed by a fixed capability and optional
// open error (for tests).
type StaticFactory struct {
	cap  contract.CapabilityName
	open func(ctx contract.OperationContext) (contract.CapabilityHandle, error)
}

// NewStaticFactory returns a factory for cap whose Open returns a no-op handle
// (or err when supplied).
func NewStaticFactory(cap contract.CapabilityName, err error) ProviderFactory {
	return &StaticFactory{cap: cap, open: func(ctx contract.OperationContext) (contract.CapabilityHandle, error) {
		if err != nil {
			return nil, err
		}
		return noopHandle{name: cap}, nil
	}}
}

func (f *StaticFactory) Capability() contract.CapabilityName { return f.cap }
func (f *StaticFactory) Open(ctx contract.OperationContext) (contract.CapabilityHandle, error) {
	return f.open(ctx)
}

type noopHandle struct{ name contract.CapabilityName }

func (h noopHandle) Name() contract.CapabilityName    { return h.name }
func (h noopHandle) Scope() contract.GrantScope       { return contract.GrantScopeComponent }
func (noopHandle) Revoke()                            {}
func (noopHandle) Evidence() contract.CleanupEvidence { return contract.CleanupEvidence{} }
