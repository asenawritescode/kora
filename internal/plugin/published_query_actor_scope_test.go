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

func (e recordingActorQueryExecutor) ExecutePublishedQuery(context.Context, contract.ModuleExecution, contract.PublishedQuery, QueryPageRequest) (contract.QueryResult, error) {
	*e.seen = append(*e.seen, PublishedQueryActorScope{SubjectID: "<none>"})
	return e.result, nil
}

func (e recordingActorQueryExecutor) ExecutePublishedQueryForActor(_ context.Context, _ contract.ModuleExecution, _ contract.PublishedQuery, actor PublishedQueryActorScope, _ QueryPageRequest) (contract.QueryResult, error) {
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
			got, err := plan.ExecutePublishedQuery(context.Background(), PublishedQueryKey{Scope: scope, OwnerModuleID: "pos.core", Name: "sessions"}, &tc.actor, QueryPageRequest{Input: json.RawMessage(`{}`), Limit: 10})
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

type pageRecordingExecutor struct {
	result contract.QueryResult
	route  *string
	page   *QueryPageRequest
}

func (e pageRecordingExecutor) ExecutePublishedQuery(_ context.Context, _ contract.ModuleExecution, _ contract.PublishedQuery, page QueryPageRequest) (contract.QueryResult, error) {
	*e.route, *e.page = "plain", page
	return e.result, nil
}

func (e pageRecordingExecutor) ExecutePublishedQueryForActor(_ context.Context, _ contract.ModuleExecution, _ contract.PublishedQuery, _ PublishedQueryActorScope, page QueryPageRequest) (contract.QueryResult, error) {
	*e.route, *e.page = "actor", page
	return e.result, nil
}

// TestExecutePublishedQueryRequestDefaults pins what the zero values of the
// single entry point mean: a nil actor reads without one, an actor routes to
// the scoped executor, and a zero limit selects the query page limit.
func TestExecutePublishedQueryRequestDefaults(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	in, out := contract.SchemaRef{ID: "till.input", Version: 1}, contract.SchemaRef{ID: "till.output", Version: 1}
	result := contract.QueryResult{Data: json.RawMessage(`[{"name":"TS-1","cashier":"cashier-1"}]`), SourceRevision: "r1", AsOf: time.Now().UTC(), Freshness: contract.QueryFresh}
	query := contract.PublishedQuery{OwnerModuleID: "pos.core", Name: "sessions", Capability: "pos.till", Operation: "sessions", InputSchema: in, OutputSchema: out, ScopePolicy: contract.ScopeTenantShared, AllowedFields: []string{"cashier", "name"}, PageLimit: 10, OwnerField: "cashier"}
	module := contract.ModuleDescriptor{ID: query.OwnerModuleID, Version: "1.0.0", Provides: []contract.CapabilityOffer{{Name: query.Capability, Contract: out, Operations: []contract.OperationSpec{{Name: query.Operation, Input: in, Output: out, Effect: contract.EffectQuery}}}}, Queries: []contract.PublishedQuery{query}}
	var route string
	var seen QueryPageRequest
	provider := ProviderRegistration{TenantID: scope.TenantID, ProviderID: "pos-provider", OwnerModuleID: module.ID, Kind: ProviderInternal, Offers: module.Provides, Queries: map[string]PublishedQueryExecutor{query.Name: pageRecordingExecutor{result: result, route: &route, page: &seen}}, ValidateSchema: func(contract.SchemaRef, []byte) error { return nil }}
	plan, err := ResolveModules(scope, []contract.ModuleDescriptor{module}, []ProviderRegistration{provider}, []ProviderSelection{{Key: BindingKey{Scope: scope, ConsumerModuleID: module.ID, CapabilityName: query.Capability}, ProviderID: provider.ProviderID}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	owner := PublishedQueryActorScope{BusinessScope: scope, SubjectID: "cashier-1", OwnerOnly: true, OwnerField: "cashier"}
	tests := []struct {
		name      string
		actor     *PublishedQueryActorScope
		page      QueryPageRequest
		wantRoute string
		wantLimit uint16
	}{
		{"nil actor with zero limit", nil, QueryPageRequest{Input: json.RawMessage(`{}`)}, "plain", 10},
		{"nil actor keeps an explicit limit and cursor", nil, QueryPageRequest{Input: json.RawMessage(`{}`), Cursor: "c1", Limit: 3}, "plain", 3},
		{"owner actor with zero limit", &owner, QueryPageRequest{Input: json.RawMessage(`{}`)}, "actor", 10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			route, seen = "", QueryPageRequest{}
			if _, err := plan.ExecutePublishedQuery(context.Background(), PublishedQueryKey{Scope: scope, OwnerModuleID: query.OwnerModuleID, Name: query.Name}, tc.actor, tc.page); err != nil {
				t.Fatal(err)
			}
			if route != tc.wantRoute || seen.Limit != tc.wantLimit || seen.Cursor != tc.page.Cursor || string(seen.Input) != `{}` {
				t.Fatalf("route %q page %+v, want route %q limit %d", route, seen, tc.wantRoute, tc.wantLimit)
			}
		})
	}
}
