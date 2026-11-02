package plugin

import (
	"fmt"
	"sort"

	"github.com/asenawritescode/kora/contract"
)

type ProviderKind string

const (
	ProviderInternal ProviderKind = "internal"
	ProviderExternal ProviderKind = "external"
)

type BindingKey struct {
	Scope            contract.BusinessScope
	ConsumerModuleID contract.ModuleID
	CapabilityName   contract.CapabilityName
}
type ProviderRegistration struct {
	TenantID      contract.TenantID
	Scope         contract.BusinessScope
	ProviderID    string
	OwnerModuleID contract.ModuleID
	Kind          ProviderKind
	Offers        []contract.CapabilityOffer
	Factories     map[contract.CapabilityName]ProviderFactory
}
type ProviderSelection struct {
	Key        BindingKey
	ProviderID string
}
type ResolvedBinding struct {
	Key        BindingKey
	ProviderID string
	Contract   contract.SchemaRef
	Revision   uint64
	Factory    ProviderFactory
}

func (b ResolvedBinding) Open(ctx contract.OperationContext) (contract.CapabilityHandle, error) {
	if b.Factory == nil {
		return nil, fmt.Errorf("provider %q is unavailable", b.ProviderID)
	}
	if ctx.Site != string(b.Key.Scope.TenantID) {
		return nil, fmt.Errorf("capability binding tenant does not match execution")
	}
	if ctx.Scope != b.Key.Scope || ctx.Scope.Validate() != nil {
		return nil, fmt.Errorf("capability binding business scope does not match execution")
	}
	return b.Factory.Open(ctx)
}

func (p BindingPlan) Resolve(key BindingKey) (ResolvedBinding, bool) {
	for _, binding := range p.Bindings {
		if binding.Key == key {
			return binding, true
		}
	}
	return ResolvedBinding{}, false
}

type BindingDiagnostic struct {
	Key  BindingKey
	Code string
}
type BindingPlan struct {
	Revision uint64
	Bindings []ResolvedBinding
	Blockers []BindingDiagnostic
	Warnings []BindingDiagnostic
}

// ResolveModules produces a deterministic candidate plan. It does not open
// providers; health is checked only for selected providers during generation assembly.
func ResolveModules(scope contract.BusinessScope, modules []contract.ModuleDescriptor, providers []ProviderRegistration, selections []ProviderSelection, revision uint64) (BindingPlan, error) {
	if scope.Validate() != nil {
		return BindingPlan{}, fmt.Errorf("binding requires explicit tenant and scope")
	}
	plan := BindingPlan{Revision: revision}
	selected := map[BindingKey]string{}
	for _, selection := range selections {
		if selection.Key.Scope.TenantID != scope.TenantID {
			return plan, fmt.Errorf("provider selection crosses tenant")
		}
		if selection.Key.Scope != scope {
			continue
		}
		selected[selection.Key] = selection.ProviderID
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].ProviderID < providers[j].ProviderID })
	for _, module := range modules {
		for _, requirement := range module.Requires {
			key := BindingKey{Scope: scope, ConsumerModuleID: module.ID, CapabilityName: requirement.Name}
			var matches []struct {
				provider ProviderRegistration
				offer    contract.CapabilityOffer
			}
			for _, provider := range providers {
				if provider.TenantID != scope.TenantID {
					continue
				}
				if provider.Scope.TenantID != "" && provider.Scope != scope {
					continue
				}
				if provider.Scope.TenantID == "" && (provider.Scope.Kind != "" || provider.Scope.LegalEntityID != "") {
					continue
				}
				if provider.ProviderID == "" || provider.OwnerModuleID == "" || (provider.Kind != ProviderInternal && provider.Kind != ProviderExternal) {
					continue
				}
				for _, offer := range provider.Offers {
					if offer.Name == requirement.Name && offer.Contract == requirement.Contract {
						matches = append(matches, struct {
							provider ProviderRegistration
							offer    contract.CapabilityOffer
						}{provider, offer})
					}
				}
			}
			if len(matches) == 0 {
				d := BindingDiagnostic{Key: key, Code: "provider_missing_or_contract_mismatch"}
				if requirement.Required {
					plan.Blockers = append(plan.Blockers, d)
				} else {
					plan.Warnings = append(plan.Warnings, d)
				}
				continue
			}
			providerID := selected[key]
			if providerID == "" && len(matches) > 1 {
				plan.Blockers = append(plan.Blockers, BindingDiagnostic{Key: key, Code: "provider_selection_required"})
				continue
			}
			if providerID == "" {
				providerID = matches[0].provider.ProviderID
			}
			var chosen *ProviderRegistration
			var offer contract.CapabilityOffer
			for i := range matches {
				if matches[i].provider.ProviderID == providerID {
					chosen = &matches[i].provider
					offer = matches[i].offer
					break
				}
			}
			if chosen == nil {
				return plan, fmt.Errorf("selected provider %q does not offer required capability", providerID)
			}
			if chosen.OwnerModuleID == module.ID {
				return plan, fmt.Errorf("module cannot bind its own provided capability")
			}
			factory := chosen.Factories[requirement.Name]
			if factory == nil {
				if requirement.Required {
					plan.Blockers = append(plan.Blockers, BindingDiagnostic{Key: key, Code: "provider_unavailable"})
				} else {
					plan.Warnings = append(plan.Warnings, BindingDiagnostic{Key: key, Code: "provider_unavailable"})
				}
				continue
			}
			plan.Bindings = append(plan.Bindings, ResolvedBinding{Key: key, ProviderID: chosen.ProviderID, Contract: offer.Contract, Revision: revision, Factory: factory})
		}
	}
	sort.Slice(plan.Bindings, func(i, j int) bool {
		a, b := plan.Bindings[i].Key, plan.Bindings[j].Key
		if a.ConsumerModuleID != b.ConsumerModuleID {
			return a.ConsumerModuleID < b.ConsumerModuleID
		}
		return a.CapabilityName < b.CapabilityName
	})
	sort.Slice(plan.Blockers, func(i, j int) bool {
		return plan.Blockers[i].Key.ConsumerModuleID < plan.Blockers[j].Key.ConsumerModuleID
	})
	sort.Slice(plan.Warnings, func(i, j int) bool {
		return plan.Warnings[i].Key.ConsumerModuleID < plan.Warnings[j].Key.ConsumerModuleID
	})
	return plan, nil
}
