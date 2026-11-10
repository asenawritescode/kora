package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
)

type testPublishedQueryExecutor struct{ result contract.QueryResult }

func (e testPublishedQueryExecutor) ExecutePublishedQuery(_ context.Context, _ contract.ModuleExecution, _ contract.PublishedQuery, _ QueryPageRequest) (contract.QueryResult, error) {
	return e.result, nil
}

type actorScopedTestQueryExecutor struct {
	result contract.QueryResult
	ignore bool
}

func (e actorScopedTestQueryExecutor) ExecutePublishedQuery(ctx context.Context, execution contract.ModuleExecution, query contract.PublishedQuery, _ QueryPageRequest) (contract.QueryResult, error) {
	return e.result, nil
}

func (e actorScopedTestQueryExecutor) ExecutePublishedQueryForActor(_ context.Context, _ contract.ModuleExecution, _ contract.PublishedQuery, actor PublishedQueryActorScope, page QueryPageRequest) (contract.QueryResult, error) {
	limit := page.Limit
	if e.ignore {
		return e.result, nil
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(e.result.Data, &rows); err != nil {
		return contract.QueryResult{}, err
	}
	visible := make([]map[string]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		if actor.OwnerOnly {
			var subject string
			if json.Unmarshal(row[actor.OwnerField], &subject) == nil && subject == actor.SubjectID {
				visible = append(visible, row)
				continue
			}
		}
		for _, field := range actor.AssignedFields {
			var subject string
			if json.Unmarshal(row[field], &subject) == nil && subject == actor.SubjectID {
				visible = append(visible, row)
				break
			}
		}
	}
	if len(visible) > int(limit) {
		visible = visible[:limit]
	}
	data, err := json.Marshal(visible)
	e.result.Data = data
	return e.result, err
}

func TestPublishedQueryAppliesActorAssignmentBeforePageLimit(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	in, out := contract.SchemaRef{ID: "task.input", Version: 1}, contract.SchemaRef{ID: "task.output", Version: 1}
	query := contract.PublishedQuery{OwnerModuleID: "sales.orders", Name: "assigned", Capability: "sales.tasks", Operation: "list", InputSchema: in, OutputSchema: out, ScopePolicy: contract.ScopeEntityOwned, AllowedFields: []string{"assigned_to", "id", "owner_id"}, PageLimit: 1}
	operation := contract.OperationSpec{Name: query.Operation, Input: in, Output: out, Effect: contract.EffectQuery}
	module := contract.ModuleDescriptor{ID: query.OwnerModuleID, Version: "1.0.0", Provides: []contract.CapabilityOffer{{Name: query.Capability, Contract: out, Operations: []contract.OperationSpec{operation}}}, Queries: []contract.PublishedQuery{query}}
	result := contract.QueryResult{Data: json.RawMessage(`[{"id":"other","assigned_to":"bob","owner_id":"bob"},{"id":"mine","assigned_to":"alice","owner_id":"alice"}]`), SnapshotToken: "snapshot-1", SourceRevision: "r1", AsOf: time.Now().UTC(), Freshness: contract.QueryFresh}
	provider := ProviderRegistration{TenantID: scope.TenantID, ProviderID: "sales-provider", OwnerModuleID: module.ID, Kind: ProviderInternal, Offers: module.Provides, Queries: map[string]PublishedQueryExecutor{query.Name: actorScopedTestQueryExecutor{result: result}}, ValidateSchema: func(_ contract.SchemaRef, data []byte) error {
		if !json.Valid(data) {
			return errors.New("invalid json")
		}
		return nil
	}}
	selection := ProviderSelection{Key: BindingKey{Scope: scope, ConsumerModuleID: module.ID, CapabilityName: query.Capability}, ProviderID: provider.ProviderID}
	plan, err := ResolveModules(scope, []contract.ModuleDescriptor{module}, []ProviderRegistration{provider}, []ProviderSelection{selection}, 1)
	if err != nil {
		t.Fatal("resolve scoped query:", err)
	}
	actor := PublishedQueryActorScope{BusinessScope: scope, SubjectID: "alice", AssignedFields: []string{"assigned_to"}}
	got, err := plan.ExecutePublishedQuery(context.Background(), PublishedQueryKey{Scope: scope, OwnerModuleID: module.ID, Name: query.Name}, &actor, QueryPageRequest{Input: json.RawMessage(`{}`), Limit: 1})
	if err != nil || string(got.Data) != `[{"assigned_to":"alice","id":"mine","owner_id":"alice"}]` {
		t.Fatalf("actor-scoped page = %s, error=%v", got.Data, err)
	}
	query.OwnerField = "owner_id"
	module.Queries[0] = query
	provider.Queries[query.Name] = actorScopedTestQueryExecutor{result: result}
	plan, err = ResolveModules(scope, []contract.ModuleDescriptor{module}, []ProviderRegistration{provider}, []ProviderSelection{selection}, 2)
	if err != nil {
		t.Fatal("resolve owner-scoped query:", err)
	}
	ownerScope := PublishedQueryActorScope{BusinessScope: scope, SubjectID: "alice", OwnerOnly: true, OwnerField: query.OwnerField}
	got, err = plan.ExecutePublishedQuery(context.Background(), PublishedQueryKey{Scope: scope, OwnerModuleID: module.ID, Name: query.Name}, &ownerScope, QueryPageRequest{Input: json.RawMessage(`{}`), Limit: 1})
	if err != nil || string(got.Data) != `[{"assigned_to":"alice","id":"mine","owner_id":"alice"}]` {
		t.Fatalf("owner-scoped page = %s, error=%v", got.Data, err)
	}
	provider.Queries[query.Name] = actorScopedTestQueryExecutor{result: result, ignore: true}
	plan, err = ResolveModules(scope, []contract.ModuleDescriptor{module}, []ProviderRegistration{provider}, []ProviderSelection{selection}, 3)
	if err != nil {
		t.Fatal("resolve misbehaving scoped query:", err)
	}
	if _, err := plan.ExecutePublishedQuery(context.Background(), PublishedQueryKey{Scope: scope, OwnerModuleID: module.ID, Name: query.Name}, &actor, QueryPageRequest{Input: json.RawMessage(`{}`), Limit: 1}); err == nil {
		t.Fatal("provider returned a row outside the verified actor assignment")
	}
}

func TestPublishedQueryUsesSameBoundedContractForInternalAndExternalProviders(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	inputSchema, outputSchema := contract.SchemaRef{ID: "report.input", Version: 1}, contract.SchemaRef{ID: "report.output", Version: 1}
	query := contract.PublishedQuery{OwnerModuleID: "sales.orders", Name: "balances", Capability: "sales.report", Operation: "list", InputSchema: inputSchema, OutputSchema: outputSchema, ScopePolicy: contract.ScopeEntityOwned, AllowedFields: []string{"amount", "id"}, PageLimit: 10}
	queryOperation := contract.OperationSpec{Name: query.Operation, Input: inputSchema, Output: outputSchema, Effect: contract.EffectQuery}
	module := contract.ModuleDescriptor{ID: "sales.orders", Version: "1.0.0", Provides: []contract.CapabilityOffer{{Name: query.Capability, Contract: outputSchema, Operations: []contract.OperationSpec{queryOperation}}}, Queries: []contract.PublishedQuery{query}}
	result := contract.QueryResult{Data: json.RawMessage(`[{"amount":12,"id":"fact-1"}]`), SourceRevision: "rev-4", AsOf: time.Now().UTC(), Freshness: contract.QueryFresh}
	for _, kind := range []ProviderKind{ProviderInternal, ProviderExternal} {
		provider := ProviderRegistration{TenantID: scope.TenantID, ProviderID: string(kind) + "-provider", OwnerModuleID: module.ID, Kind: kind, Offers: []contract.CapabilityOffer{{Name: query.Capability, Contract: outputSchema, Operations: []contract.OperationSpec{queryOperation}}}, Queries: map[string]PublishedQueryExecutor{"balances": testPublishedQueryExecutor{result}}, ValidateSchema: func(_ contract.SchemaRef, b []byte) error {
			if !json.Valid(b) {
				return errors.New("invalid JSON")
			}
			return nil
		}}
		selection := ProviderSelection{Key: BindingKey{Scope: scope, ConsumerModuleID: module.ID, CapabilityName: query.Capability}, ProviderID: provider.ProviderID}
		plan, err := ResolveModules(scope, []contract.ModuleDescriptor{module}, []ProviderRegistration{provider}, []ProviderSelection{selection}, 4)
		if err != nil {
			t.Fatalf("%s resolve: %v", kind, err)
		}
		got, err := plan.ExecutePublishedQuery(context.Background(), PublishedQueryKey{Scope: scope, OwnerModuleID: module.ID, Name: query.Name}, nil, QueryPageRequest{Input: json.RawMessage(`{}`), Limit: 5})
		if err != nil || string(got.Data) != string(result.Data) {
			t.Fatalf("%s query = %#v, %v", kind, got, err)
		}
		if _, overLimitErr := plan.ExecutePublishedQuery(context.Background(), PublishedQueryKey{Scope: scope, OwnerModuleID: module.ID, Name: query.Name}, nil, QueryPageRequest{Input: json.RawMessage(`{}`), Limit: 11}); overLimitErr == nil {
			t.Fatalf("%s query exceeded page limit", kind)
		}
		bad := testPublishedQueryExecutor{contract.QueryResult{Data: json.RawMessage(`[{"secret":true}]`), AsOf: time.Now().UTC(), Freshness: contract.QueryFresh}}
		provider.Queries[query.Name] = bad
		plan, err = ResolveModules(scope, []contract.ModuleDescriptor{module}, []ProviderRegistration{provider}, []ProviderSelection{selection}, 4)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = plan.ExecutePublishedQuery(context.Background(), PublishedQueryKey{Scope: scope, OwnerModuleID: module.ID, Name: query.Name}, nil, QueryPageRequest{Input: json.RawMessage(`{}`), Limit: 5}); err == nil {
			t.Fatalf("%s provider leaked an undeclared field", kind)
		}
		provider.Offers = nil
		unoffered, err := ResolveModules(scope, []contract.ModuleDescriptor{module}, []ProviderRegistration{provider}, []ProviderSelection{selection}, 4)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := unoffered.ExecutePublishedQuery(context.Background(), PublishedQueryKey{Scope: scope, OwnerModuleID: module.ID, Name: query.Name}, nil, QueryPageRequest{Input: json.RawMessage(`{}`), Limit: 5}); err == nil {
			t.Fatalf("%s query executor without the capability offer was accepted", kind)
		}
	}
}
