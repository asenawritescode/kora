// Package plugin resolves module capability bindings and executes published provider
// operations. Providers are exposed to callers as capability handles, so removing a
// provider degrades only its dependents.
package plugin

import "github.com/asenawritescode/kora/contract"

// ProviderFactory exposes one provider as a capability handle.
type ProviderFactory interface {
	Capability() contract.CapabilityName
	Open(ctx contract.OperationContext) (contract.CapabilityHandle, error)
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
