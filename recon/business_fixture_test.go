package recon

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/plugin"
)

type fixtureBusinessQueryExecutor struct{ data json.RawMessage }

func (e fixtureBusinessQueryExecutor) ExecutePublishedQuery(context.Context, contract.ModuleExecution, contract.PublishedQuery, json.RawMessage, string, uint16) (contract.QueryResult, error) {
	return contract.QueryResult{Data: append(json.RawMessage(nil), e.data...), SnapshotToken: "fixture-snapshot", SourceRevision: "fixture-revision", AsOf: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC), Freshness: contract.QueryFresh}, nil
}

func TestProgrammaticInvoiceSettlementAndJournalReconciliationAdapters(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-fixture", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	in, out := contract.SchemaRef{ID: "fixture.recon.input", Version: 1}, contract.SchemaRef{ID: "fixture.recon.output", Version: 1}
	allFields := []string{"key", "source_ref", "target_ref", "entity_id", "amount", "currency", "occurred_at", "lifecycle"}
	fixtureQueries := []struct {
		owner, name, capability string
		data                    json.RawMessage
	}{
		{"sales.orders", "billable_sales", "sales.reconcile", json.RawMessage(`[{"key":"sale:1","source_ref":"sale:1","entity_id":"entity-a","amount":"12.50","currency":"KES","occurred_at":"2026-11-02T08:00:00Z"}]`)},
		{"accounts.receivable", "issued_invoices", "ar.reconcile", json.RawMessage(`[]`)},
		{"payments.settlement", "settlement_obligations", "settlement.reconcile", json.RawMessage(`[{"key":"payment:1","source_ref":"payment:1","entity_id":"entity-a","amount":"40.00","currency":"KES","occurred_at":"2026-11-02T08:00:00Z"}]`)},
		{"bank.reconciliation", "bank_settlements", "bank.reconcile", json.RawMessage(`[{"key":"payment:1","target_ref":"settlement:1","entity_id":"entity-a","amount":"40.00","currency":"KES","occurred_at":"2026-11-02T09:00:00Z"},{"key":"payment:1","target_ref":"settlement:2","entity_id":"entity-a","amount":"40.00","currency":"KES","occurred_at":"2026-11-02T09:30:00Z"}]`)},
		{"inventory.valuation", "posting_intents", "inventory.reconcile", json.RawMessage(`[{"key":"posting:1","source_ref":"valuation:1","entity_id":"entity-a","amount":"125.00","currency":"KES","occurred_at":"2026-11-02T08:00:00Z"}]`)},
		{"accounting.ledger", "journal_postings", "ledger.reconcile", json.RawMessage(`[{"key":"posting:1","target_ref":"journal:1","entity_id":"entity-a","amount":"120.00","currency":"KES","occurred_at":"2026-11-02T09:00:00Z"}]`)},
	}
	modules := []contract.ModuleDescriptor{{ID: "finance.reconciliation", Version: "1.0.0"}}
	providers := make([]plugin.ProviderRegistration, 0, len(fixtureQueries))
	selections := make([]plugin.ProviderSelection, 0, len(fixtureQueries))
	for _, fixture := range fixtureQueries {
		query := contract.PublishedQuery{OwnerModuleID: contract.ModuleID(fixture.owner), Name: fixture.name, Capability: contract.CapabilityName(fixture.capability), Operation: "list", InputSchema: in, OutputSchema: out, ScopePolicy: contract.ScopeEntityOwned, AllowedFields: allFields, PageLimit: 10}
		offer := contract.CapabilityOffer{Name: query.Capability, Contract: out, Operations: []contract.OperationSpec{{Name: query.Operation, Input: in, Output: out, Effect: contract.EffectQuery}}}
		module := contract.ModuleDescriptor{ID: contract.ModuleID(fixture.owner), Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}, Queries: []contract.PublishedQuery{query}}
		modules = append(modules, module)
		providerID := fixture.owner + "-internal"
		providers = append(providers, plugin.ProviderRegistration{TenantID: scope.TenantID, ProviderID: providerID, OwnerModuleID: module.ID, Kind: plugin.ProviderInternal, Offers: []contract.CapabilityOffer{offer}, Queries: map[string]plugin.PublishedQueryExecutor{query.Name: fixtureBusinessQueryExecutor{data: fixture.data}}, ValidateSchema: func(_ contract.SchemaRef, data []byte) error {
			if !json.Valid(data) {
				return fmt.Errorf("invalid fixture query JSON")
			}
			return nil
		}})
		selections = append(selections, plugin.ProviderSelection{Key: plugin.BindingKey{Scope: scope, ConsumerModuleID: module.ID, CapabilityName: query.Capability}, ProviderID: providerID})
	}
	plan, err := plugin.ResolveModules(scope, modules, providers, selections, 7)
	if err != nil {
		t.Fatal("resolve fresh programmatic fixture domains:", err)
	}
	cutoff := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	// The fixture records a successful transport receipt for sale:1 while the
	// AR owner's published query returns no invoice. The comparison reads only
	// business owner queries, so the receipt cannot hide the missing effect.
	deliveredReceipts := map[string]bool{"sale:1": true}
	rules := []struct {
		name, sourceOwner, sourceQuery, targetOwner, targetQuery, key string
		kind                                                          BusinessFindingKind
	}{
		{"sales to AR invoice omission after delivered receipt", "sales.orders", "billable_sales", "accounts.receivable", "issued_invoices", "sale:1", FindingMissingEffect},
		{"duplicate settlement", "payments.settlement", "settlement_obligations", "bank.reconciliation", "bank_settlements", "payment:1", FindingDuplicateEffect},
		{"inventory posting amount mismatch", "inventory.valuation", "posting_intents", "accounting.ledger", "journal_postings", "posting:1", FindingAmountMismatch},
	}
	for _, fixture := range rules {
		t.Run(fixture.name, func(t *testing.T) {
			if fixture.kind == FindingMissingEffect && !deliveredReceipts[fixture.key] {
				t.Fatal("fixture must have successful delivery receipt before reconciliation")
			}
			rule := BusinessReconciliationRule{OwnerModuleID: "finance.reconciliation", ID: fixture.name, Version: 1, Scope: scope, MaxRows: 100,
				SourceQuery: BusinessQueryRef{OwnerModuleID: contract.ModuleID(fixture.sourceOwner), Name: fixture.sourceQuery, Input: json.RawMessage(`{}`), PageLimit: 10},
				TargetQuery: BusinessQueryRef{OwnerModuleID: contract.ModuleID(fixture.targetOwner), Name: fixture.targetQuery, Input: json.RawMessage(`{}`), PageLimit: 10}}
			result, _, _, err := ExecuteBusinessReconciliation(context.Background(), rule, cutoff, BusinessRunCursor{}, PublishedBusinessQueryReader(plan))
			if err != nil || !result.Complete || len(result.Findings) != 1 || result.Findings[0].Kind != fixture.kind || result.Findings[0].Key != fixture.key {
				t.Fatalf("fixture reconciliation result = %+v, %v", result, err)
			}
		})
	}
}
