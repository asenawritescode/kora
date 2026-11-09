package plugin

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
)

type recordingActorQueryExecutor struct {
	result contract.QueryResult
	seen   *[]PublishedQueryActorScope
}

func (e recordingActorQueryExecutor) ExecutePublishedQuery(context.Context, contract.ModuleExecution, contract.PublishedQuery, json.RawMessage, string, uint16) (contract.QueryResult, error) {
	*e.seen = append(*e.seen, PublishedQueryActorScope{SubjectID: "<none>"})
	return e.result, nil
}

func (e recordingActorQueryExecutor) ExecutePublishedQueryForActor(_ context.Context, _ contract.ModuleExecution, _ contract.PublishedQuery, actor PublishedQueryActorScope, _ json.RawMessage, _ string, _ uint16) (contract.QueryResult, error) {
	*e.seen = append(*e.seen, actor)
	return e.result, nil
}

// TestPublishedQueryVerifiedActorWithoutRowScope covers #407: an owner_field
// query reached through a plain grant or the Administrator bypass carries the
// verified actor (OwnerOnly=false) and is not row-filtered by the plan.
func TestPublishedQueryVerifiedActorWithoutRowScope(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	other := contract.BusinessScope{TenantID: "tenant-b", Kind: contract.ScopeKindTenant}
	in, out := contract.SchemaRef{ID: "till.input", Version: 1}, contract.SchemaRef{ID: "till.output", Version: 1}
	rows := json.RawMessage(`[{"name":"TS-1","cashier":"cashier-1"},{"name":"TS-2","cashier":"cashier-2"}]`)
	result := contract.QueryResult{Data: rows, SourceRevision: "r1", AsOf: time.Now().UTC(), Freshness: contract.QueryFresh}
	resolve := func(t *testing.T, ownerField string, executor PublishedQueryExecutor) BindingPlan {
		t.Helper()
		query := contract.PublishedQuery{OwnerModuleID: "pos.core", Name: "sessions", Capability: "pos.till", Operation: "sessions", InputSchema: in, OutputSchema: out, ScopePolicy: contract.ScopeTenantShared, AllowedFields: []string{"cashier", "name"}, PageLimit: 10, OwnerField: ownerField}
		module := contract.ModuleDescriptor{ID: query.OwnerModuleID, Version: "1.0.0", Provides: []contract.CapabilityOffer{{Name: query.Capability, Contract: out, Operations: []contract.OperationSpec{{Name: query.Operation, Input: in, Output: out, Effect: contract.EffectQuery}}}}, Queries: []contract.PublishedQuery{query}}
		provider := ProviderRegistration{TenantID: scope.TenantID, ProviderID: "pos-provider", OwnerModuleID: module.ID, Kind: ProviderInternal, Offers: module.Provides, Queries: map[string]PublishedQueryExecutor{query.Name: executor}, ValidateSchema: func(contract.SchemaRef, []byte) error { return nil }}
		plan, err := ResolveModules(scope, []contract.ModuleDescriptor{module}, []ProviderRegistration{provider}, []ProviderSelection{{Key: BindingKey{Scope: scope, ConsumerModuleID: module.ID, CapabilityName: query.Capability}, ProviderID: provider.ProviderID}}, 1)
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	verified := PublishedQueryActorScope{BusinessScope: scope, SubjectID: "admin-1", OwnerField: "cashier"}
	tests := []struct {
		name       string
		ownerField string
		scoped     bool
		actor      PublishedQueryActorScope
		wantErr    bool
		wantSeen   string // subject the executor received; "<none>" is the unscoped fallback
	}{
		{"verified actor reaches scoped executor unfiltered", "cashier", true, verified, false, "admin-1"},
		{"verified actor falls back for unscoped executor", "cashier", false, verified, false, "<none>"},
		{"verified actor on query without owner_field", "", true, PublishedQueryActorScope{BusinessScope: scope, SubjectID: "admin-1"}, true, ""},
		{"verified actor owner field mismatch", "cashier", true, PublishedQueryActorScope{BusinessScope: scope, SubjectID: "admin-1", OwnerField: "owner"}, true, ""},
		{"verified actor without subject", "cashier", true, PublishedQueryActorScope{BusinessScope: scope, OwnerField: "cashier"}, true, ""},
		{"verified actor for another scope", "cashier", true, PublishedQueryActorScope{BusinessScope: other, SubjectID: "admin-1", OwnerField: "cashier"}, true, ""},
		{"owner only still rejects foreign rows", "cashier", true, PublishedQueryActorScope{BusinessScope: scope, SubjectID: "cashier-1", OwnerOnly: true, OwnerField: "cashier"}, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var seen []PublishedQueryActorScope
			var executor PublishedQueryExecutor = recordingActorQueryExecutor{result: result, seen: &seen}
			if !tc.scoped {
				executor = testPublishedQueryExecutor{result: result}
			}
			plan := resolve(t, tc.ownerField, executor)
			got, err := plan.ExecutePublishedQueryForActor(context.Background(), scope, "pos.core", "sessions", tc.actor, json.RawMessage(`{}`), "", 10)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if string(got.Data) != string(rows) {
				t.Fatalf("rows = %s", got.Data)
			}
			if tc.scoped && (len(seen) != 1 || seen[0].SubjectID != tc.wantSeen || seen[0].OwnerOnly) {
				t.Fatalf("executor saw %#v, want subject %q", seen, tc.wantSeen)
			}
		})
	}
}
