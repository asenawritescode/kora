package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	_ "github.com/mattn/go-sqlite3"
)

func TestSQLPublishedQueryProvider_PinnedSnapshotReplayRestartAndScopeIsolation(t *testing.T) {
	ctx := context.Background()
	database, err := sql.Open("sqlite3", t.TempDir()+"/published-query.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)

	dialect := db.Resolve("libsql")
	provider, err := NewSQLPublishedQueryProvider(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}

	inputSchema := contract.SchemaRef{ID: "sales.orders.input", Version: 1}
	outputSchema := contract.SchemaRef{ID: "sales.orders.output", Version: 1}
	query := contract.PublishedQuery{
		OwnerModuleID: "sales.orders", Name: "mine", Capability: "sales.orders.read", Operation: "list",
		InputSchema: inputSchema, OutputSchema: outputSchema, ScopePolicy: contract.ScopeEntityOwned,
		AllowedFields: []string{"id", "owner_id", "amount"}, OwnerField: "owner_id", PageLimit: 2,
		OfflineEligible: true, OfflineFactKey: "id",
	}
	module := contract.ModuleDescriptor{
		ID: query.OwnerModuleID, Version: "1.0.0",
		Provides: []contract.CapabilityOffer{{Name: query.Capability, Contract: outputSchema, Operations: []contract.OperationSpec{{Name: query.Operation, Input: inputSchema, Output: outputSchema, Effect: contract.EffectQuery}}}},
		Queries:  []contract.PublishedQuery{query},
	}
	scopeA := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	scopeB := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-b"}
	registration := ProviderRegistration{
		TenantID: scopeA.TenantID, ProviderID: "engine.published-facts", OwnerModuleID: module.ID, Kind: ProviderInternal,
		Offers: module.Provides, Queries: map[string]PublishedQueryExecutor{query.Name: provider},
		ValidateSchema: func(_ contract.SchemaRef, raw []byte) error {
			if !json.Valid(raw) {
				return fmt.Errorf("invalid JSON")
			}
			return nil
		},
	}
	input := json.RawMessage(`{"status":"open"}`)
	write := func(scope contract.BusinessScope, row string) {
		t.Helper()
		tx, err := database.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		execution, err := contract.NewModuleExecution(scope, module.ID, module.ID, 7, query.Operation)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.PutFactTx(ctx, tx, FactStream{Execution: execution, Query: query, Input: input}, json.RawMessage(row)); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	remove := func(scope contract.BusinessScope, key, owner string) {
		t.Helper()
		tx, err := database.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		execution, err := contract.NewModuleExecution(scope, module.ID, module.ID, 7, query.Operation)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.DeleteFactTx(ctx, tx, FactStream{Execution: execution, Query: query, Input: input}, key, owner); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	write(scopeA, `{"id":"a-0","owner_id":"alice","amount":0}`)
	write(scopeA, `{"id":"a-1","owner_id":"alice","amount":1}`)
	write(scopeA, `{"id":"b-1","owner_id":"bob","amount":9}`)
	write(scopeB, `{"id":"a-2","owner_id":"alice","amount":8}`)

	plan, err := ResolveModules(scopeA, []contract.ModuleDescriptor{module}, []ProviderRegistration{registration}, nil, 7)
	if err != nil {
		t.Fatal(err)
	}
	actor := PublishedQueryActorScope{BusinessScope: scopeA, SubjectID: "alice", OwnerOnly: true, OwnerField: query.OwnerField}
	assignedActor := actor
	assignedActor.AssignedFields = []string{"owner_id"}
	if plan.CanReplayPublishedQueryOfflineForActor(scopeA, module.ID, query.Name, &assignedActor) {
		t.Fatal("provider advertised unsupported assignment-scoped offline replay")
	}
	unrestricted, err := plan.ExecutePublishedQuery(ctx, PublishedQueryKey{Scope: scopeA, OwnerModuleID: module.ID, Name: query.Name}, nil, QueryPageRequest{Input: input, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	unrestrictedNext, err := plan.ExecutePublishedQuery(ctx, PublishedQueryKey{Scope: scopeA, OwnerModuleID: module.ID, Name: query.Name}, nil, QueryPageRequest{Input: input, Cursor: unrestricted.NextCursor, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	allUnrestricted := string(unrestricted.Data) + string(unrestrictedNext.Data)
	if !strings.Contains(allUnrestricted, `"owner_id":"alice"`) || !strings.Contains(allUnrestricted, `"owner_id":"bob"`) || strings.Contains(allUnrestricted, `"id":"a-2"`) {
		t.Fatalf("unrestricted entity snapshot = %s", unrestricted.Data)
	}
	snapshot, err := plan.ExecutePublishedQuery(ctx, PublishedQueryKey{Scope: scopeA, OwnerModuleID: module.ID, Name: query.Name}, &actor, QueryPageRequest{Input: input, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshot.Data) != `[{"id":"a-0","owner_id":"alice","amount":0}]` || snapshot.SnapshotToken == "" || snapshot.SourceCursor == "" || snapshot.NextCursor == "" {
		t.Fatalf("snapshot = %+v", snapshot)
	}

	write(scopeA, `{"id":"a-1","owner_id":"alice","amount":2}`)
	write(scopeA, `{"id":"a-3","owner_id":"alice","amount":3}`)
	remove(scopeA, "a-1", "alice")
	secondSnapshot, err := plan.ExecutePublishedQuery(ctx, PublishedQueryKey{Scope: scopeA, OwnerModuleID: module.ID, Name: query.Name}, &actor, QueryPageRequest{Input: input, Cursor: snapshot.NextCursor, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if secondSnapshot.SnapshotToken != snapshot.SnapshotToken || secondSnapshot.SourceCursor != snapshot.SourceCursor || secondSnapshot.NextCursor != "" || string(secondSnapshot.Data) != `[{"id":"a-1","owner_id":"alice","amount":1}]` {
		t.Fatalf("second pinned snapshot page = %+v", secondSnapshot)
	}

	// Reopen the provider to prove all cursor and fact state is durable rather
	// than retained in the adapter process.
	restarted, err := NewSQLPublishedQueryProvider(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	registration.Queries[query.Name] = restarted
	plan, err = ResolveModules(scopeA, []contract.ModuleDescriptor{module}, []ProviderRegistration{registration}, nil, 7)
	if err != nil {
		t.Fatal(err)
	}
	first, err := plan.ReplayPublishedQueryForActor(ctx, PublishedQueryKey{Scope: scopeA, OwnerModuleID: module.ID, Name: query.Name}, actor, QueryReplayRequest{Input: input, Snapshot: snapshot.SnapshotToken, After: snapshot.SourceCursor, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Changes) != 2 || first.Changes[0].FactKey != "a-1" || first.Changes[1].FactKey != "a-3" || first.NextCursor == "" {
		t.Fatalf("first replay = %+v", first)
	}
	second, err := plan.ReplayPublishedQueryForActor(ctx, PublishedQueryKey{Scope: scopeA, OwnerModuleID: module.ID, Name: query.Name}, actor, QueryReplayRequest{Input: input, Snapshot: snapshot.SnapshotToken, After: snapshot.SourceCursor, Through: first.ThroughCursor, Replay: first.NextCursor, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Changes) != 1 || second.Changes[0].FactKey != "a-1" || !second.Changes[0].Deleted || second.NextCursor != "" || second.ThroughCursor != first.ThroughCursor {
		t.Fatalf("second replay = %+v", second)
	}

	// The original pinned snapshot remains unchanged after later writes.
	pinnedHigh, err := parsePublishedCursor(snapshot.SourceCursor)
	if err != nil {
		t.Fatal(err)
	}
	pinnedRows, _, err := restarted.snapshotRows(ctx, changeFilter{scope: scopeA, query: query, subject: "alice", input: input}, "", pinnedHigh, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(pinnedRows) != 2 || string(pinnedRows[0]) != `{"id":"a-0","owner_id":"alice","amount":0}` || string(pinnedRows[1]) != `{"id":"a-1","owner_id":"alice","amount":1}` {
		t.Fatalf("pinned rows = %s", pinnedRows)
	}

	for _, kind := range []ProviderKind{ProviderInternal, ProviderExternal} {
		t.Run("provider-kind-"+string(kind), func(t *testing.T) {
			candidate := registration
			candidate.ProviderID = string(kind) + ".published-facts"
			candidate.Kind = kind
			candidate.Queries = map[string]PublishedQueryExecutor{query.Name: restarted}
			kindPlan, err := ResolveModules(scopeA, []contract.ModuleDescriptor{module}, []ProviderRegistration{candidate}, nil, 7)
			if err != nil {
				t.Fatal(err)
			}
			got, err := kindPlan.ExecutePublishedQuery(ctx, PublishedQueryKey{Scope: scopeA, OwnerModuleID: module.ID, Name: query.Name}, &actor, QueryPageRequest{Input: input, Limit: 2})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(got.Data), `"id":"a-0"`) || strings.Contains(string(got.Data), `"owner_id":"bob"`) {
				t.Fatalf("%s provider result = %s", kind, got.Data)
			}
		})
	}

	beforeTransfer, err := plan.ExecutePublishedQuery(ctx, PublishedQueryKey{Scope: scopeA, OwnerModuleID: module.ID, Name: query.Name}, &actor, QueryPageRequest{Input: input, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	write(scopeA, `{"id":"a-0","owner_id":"bob","amount":10}`)
	revoked, err := plan.ReplayPublishedQueryForActor(ctx, PublishedQueryKey{Scope: scopeA, OwnerModuleID: module.ID, Name: query.Name}, actor, QueryReplayRequest{Input: input, Snapshot: beforeTransfer.SnapshotToken, After: beforeTransfer.SourceCursor, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(revoked.Changes) != 1 || revoked.Changes[0].FactKey != "a-0" || !revoked.Changes[0].Deleted {
		t.Fatalf("previous owner revocation replay = %+v", revoked)
	}
	bob := PublishedQueryActorScope{BusinessScope: scopeA, SubjectID: "bob", OwnerOnly: true, OwnerField: query.OwnerField}
	bobSnapshot, err := plan.ExecutePublishedQuery(ctx, PublishedQueryKey{Scope: scopeA, OwnerModuleID: module.ID, Name: query.Name}, &bob, QueryPageRequest{Input: input, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bobSnapshot.Data), `"id":"a-0"`) || strings.Contains(string(bobSnapshot.Data), `"id":"a-1"`) {
		t.Fatalf("new owner snapshot = %s", bobSnapshot.Data)
	}
}

func TestSQLPublishedQueryProvider_RejectsUnverifiedAndAssignedScopes(t *testing.T) {
	database, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	provider, err := NewSQLPublishedQueryProvider(database, db.Resolve("libsql"))
	if err != nil {
		t.Fatal(err)
	}
	query := contract.PublishedQuery{OwnerModuleID: "sales.orders", Name: "mine", ScopePolicy: contract.ScopeEntityOwned, OwnerField: "owner_id", OfflineEligible: true, OfflineFactKey: "id", PageLimit: 10}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	actor := PublishedQueryActorScope{BusinessScope: scope, SubjectID: "alice", OwnerOnly: true, OwnerField: "owner_id", AssignedFields: []string{"assignee_id"}}
	if _, unverifiedErr := provider.ExecutePublishedQueryForActor(context.Background(), contract.ModuleExecution{}, query, actor, QueryPageRequest{Input: json.RawMessage(`{}`), Limit: 10}); unverifiedErr == nil {
		t.Fatal("unverified execution was accepted")
	}
	execution, err := contract.NewModuleExecution(scope, query.OwnerModuleID, query.OwnerModuleID, 1, "list")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ExecutePublishedQueryForActor(context.Background(), execution, query, actor, QueryPageRequest{Input: json.RawMessage(`{}`), Limit: 10}); err == nil {
		t.Fatal("assigned scope without an owner-published visibility projection was accepted")
	}
}
