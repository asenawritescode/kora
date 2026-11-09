package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/plugin"
	"github.com/asenawritescode/kora/internal/workflow"
)

// registeredSQLPublishedQueryProviders wires the Engine-owned durable fact
// adapter only for owner-declared offline queries that do not already have a
// configured provider. Capability contracts that mix query and effectful
// operations remain explicit provider integrations; Engine never guesses how
// their non-query operations should execute.
func registeredSQLPublishedQueryProviders(ctx context.Context, tenant contract.TenantID, database *sql.DB, dialect db.Dialect, modules []contract.ModuleDescriptor, schemas workflow.PayloadSchemaRegistry, configured []plugin.ProviderRegistration) ([]plugin.ProviderRegistration, error) {
	if ctx == nil || database == nil || dialect == nil {
		return nil, fmt.Errorf("durable published query providers require tenant persistence")
	}
	provider, err := plugin.NewSQLPublishedQueryProvider(database, dialect)
	if err != nil {
		return nil, err
	}
	ordered := append([]contract.ModuleDescriptor(nil), modules...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	var registrations []plugin.ProviderRegistration
	needsSchema := false
	for _, module := range ordered {
		queries := make(map[string]plugin.PublishedQueryExecutor)
		offers := make([]contract.CapabilityOffer, 0)
		seenOffers := map[contract.CapabilityName]bool{}
		for _, query := range module.Queries {
			if !query.OfflineEligible || configuredPublishedQuery(configured, module.ID, query.Name) {
				continue
			}
			var declared *contract.CapabilityOffer
			for i := range module.Provides {
				offer := &module.Provides[i]
				if offer.Name != query.Capability {
					continue
				}
				queryOnly := len(offer.Operations) > 0
				matches := false
				for _, operation := range offer.Operations {
					queryOnly = queryOnly && operation.Effect == contract.EffectQuery
					matches = matches || operation.Name == query.Operation && operation.Input == query.InputSchema && operation.Output == query.OutputSchema && operation.Effect == contract.EffectQuery
				}
				if queryOnly && matches {
					declared = offer
				}
			}
			if declared == nil {
				continue
			}
			if schemas == nil || !schemas.Registered(query.InputSchema) || !schemas.Registered(query.OutputSchema) {
				return nil, fmt.Errorf("module %q offline query %q requires registered input and output schemas", module.ID, query.Name)
			}
			queries[query.Name] = provider
			if !seenOffers[declared.Name] {
				offers = append(offers, *declared)
				seenOffers[declared.Name] = true
			}
		}
		if len(queries) == 0 {
			continue
		}
		needsSchema = true
		registrations = append(registrations, plugin.ProviderRegistration{
			TenantID: tenant, ProviderID: "engine.published-facts." + string(module.ID), OwnerModuleID: module.ID, Kind: plugin.ProviderInternal,
			Offers: offers, Queries: queries,
			ValidateSchema: func(ref contract.SchemaRef, raw []byte) error {
				if !schemas.Registered(ref) {
					return fmt.Errorf("schema %s@%d is not registered", ref.ID, ref.Version)
				}
				return schemas.Validate(ref, json.RawMessage(raw))
			},
		})
	}
	if needsSchema {
		if err := provider.EnsureSchema(ctx); err != nil {
			return nil, err
		}
	}
	return registrations, nil
}

func configuredPublishedQuery(providers []plugin.ProviderRegistration, owner contract.ModuleID, name string) bool {
	for _, provider := range providers {
		if provider.OwnerModuleID == owner && provider.Queries[name] != nil {
			return true
		}
	}
	return false
}
