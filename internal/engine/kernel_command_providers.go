package engine

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/plugin"
	"github.com/asenawritescode/kora/internal/workflow"
)

// moduleProviderSource is the tenant state module provider registrations are
// derived from. Each registered*Providers function reads the fields it needs
// and ignores the rest: db and registry for config queries and kernel commands,
// dialect for config and durable published queries, configured for the kernel
// command and durable published query providers.
type moduleProviderSource struct {
	tenant     contract.TenantID
	db         *sql.DB
	dialect    db.Dialect
	registry   *doctype.Registry
	modules    []contract.ModuleDescriptor
	schemas    workflow.PayloadSchemaRegistry
	configured []plugin.ProviderRegistration
}

func registeredKernelCommandProviders(src moduleProviderSource, commands *kernel.CommandRegistry, commandKernel *kernel.Kernel) ([]plugin.ProviderRegistration, error) {
	var result []plugin.ProviderRegistration
	ordered := append([]contract.ModuleDescriptor(nil), src.modules...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for _, module := range ordered {
		var offers []contract.CapabilityOffer
		factories := make(map[contract.CapabilityName]plugin.ProviderFactory)
		for _, offer := range module.Provides {
			localCount := 0
			for _, operation := range offer.Operations {
				if operation.Effect == contract.EffectLocal {
					localCount++
				}
			}
			if localCount == 0 || configuredInternalOffer(src.configured, module.ID, offer) {
				continue
			}
			if localCount != len(offer.Operations) {
				return nil, fmt.Errorf("module %q capability %q mixes local command effects with other effects; register separate capability contracts", module.ID, offer.Name)
			}
			if commands == nil {
				return nil, fmt.Errorf("module %q local capability %q requires the generation command registry", module.ID, offer.Name)
			}
			if src.schemas == nil {
				return nil, fmt.Errorf("module %q local capability %q requires the registered payload schema registry", module.ID, offer.Name)
			}
			for _, operation := range offer.Operations {
				if !src.schemas.Registered(operation.Input) || !src.schemas.Registered(operation.Output) {
					return nil, fmt.Errorf("module %q capability %q operation %q references an unregistered input or output schema", module.ID, offer.Name, operation.Name)
				}
				command, ok := commands.LookupCapability(offer.Name, operation.Name)
				if !ok || command.ModuleOwnerID != module.ID || command.InputSchema != operation.Input || command.OutputSchema != operation.Output {
					return nil, fmt.Errorf("module %q capability %q operation %q has no matching owner command and schema contract", module.ID, offer.Name, operation.Name)
				}
			}
			validateSchema := func(ref contract.SchemaRef, raw []byte) error {
				if !src.schemas.Registered(ref) {
					return fmt.Errorf("schema %s@%d is not registered", ref.ID, ref.Version)
				}
				return src.schemas.Validate(ref, json.RawMessage(raw))
			}
			factory, err := newKernelCommandProviderFactory(kernelCommandProviderFactory{database: src.db, registry: src.registry, commands: commands, engine: commandKernel, tenant: src.tenant, owner: module.ID, capability: offer.Name, validateSchema: validateSchema})
			if err != nil {
				return nil, err
			}
			offers = append(offers, offer)
			factories[offer.Name] = factory
		}
		if len(offers) == 0 {
			continue
		}
		names := make([]string, 0, len(offers))
		for _, offer := range offers {
			names = append(names, string(offer.Name))
		}
		sort.Strings(names)
		digest := sha256.Sum256([]byte(string(module.ID) + "\x00" + strings.Join(names, "\x00")))
		result = append(result, plugin.ProviderRegistration{
			TenantID: src.tenant, ProviderID: "engine.internal." + hex.EncodeToString(digest[:8]),
			OwnerModuleID: module.ID, Kind: plugin.ProviderInternal, Offers: offers,
			Factories: factories, ValidateSchema: func(ref contract.SchemaRef, raw []byte) error {
				if !src.schemas.Registered(ref) {
					return fmt.Errorf("schema %s@%d is not registered", ref.ID, ref.Version)
				}
				return src.schemas.Validate(ref, json.RawMessage(raw))
			},
		})
	}
	return result, nil
}

func configuredInternalOffer(providers []plugin.ProviderRegistration, owner contract.ModuleID, offer contract.CapabilityOffer) bool {
	for _, provider := range providers {
		if provider.Kind != plugin.ProviderInternal || provider.OwnerModuleID != owner {
			continue
		}
		for _, registered := range provider.Offers {
			if registered.Name == offer.Name && registered.Contract == offer.Contract && len(registered.Operations) == len(offer.Operations) {
				matched := true
				for i := range offer.Operations {
					if registered.Operations[i] != offer.Operations[i] {
						matched = false
						break
					}
				}
				if matched {
					return true
				}
			}
		}
	}
	return false
}
