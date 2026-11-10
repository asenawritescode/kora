package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
)

// PublishedQueryKey names one published query of an owner module in a scope.
type PublishedQueryKey struct {
	Scope         contract.BusinessScope
	OwnerModuleID contract.ModuleID
	Name          string
}

// QueryPageRequest is one page of a published query: the caller's input, the
// continuation cursor ("" for the first page) and the page size (0 selects the
// query's PageLimit).
type QueryPageRequest struct {
	Input  json.RawMessage
	Cursor string
	Limit  uint16
}

// QueryReplayRequest is one page of changes after a query snapshot. The zero
// Through and Replay cursors start a new replay; Snapshot is only read by
// actor-scoped replay.
type QueryReplayRequest struct {
	Input    json.RawMessage
	Snapshot string
	After    string
	Through  string
	Replay   string
	Limit    uint16
}

// PublishedQueryExecutor reads one page of a published query without a
// verified actor.
type PublishedQueryExecutor interface {
	ExecutePublishedQuery(context.Context, contract.ModuleExecution, contract.PublishedQuery, QueryPageRequest) (contract.QueryResult, error)
}

// PublishedQueryActorScope is derived only from verified policy execution.
// Providers must apply assignment filters before pagination, not after it.
type PublishedQueryActorScope struct {
	BusinessScope  contract.BusinessScope
	SubjectID      string
	OwnerOnly      bool
	OwnerField     string
	AssignedFields []string
}

// ScopedPublishedQueryExecutor implements actor row filtering in the source
// query so limits and cursors describe only rows visible to that actor.
type ScopedPublishedQueryExecutor interface {
	ExecutePublishedQueryForActor(context.Context, contract.ModuleExecution, contract.PublishedQuery, PublishedQueryActorScope, QueryPageRequest) (contract.QueryResult, error)
}

// PublishedQueryReplayExecutor replays owner changes after an immutable query
// snapshot. The first page fixes ThroughCursor; all following pages must use
// that same high-water mark so the persisted projection cursor is gapless.
type PublishedQueryReplayExecutor interface {
	ReplayPublishedQuery(context.Context, contract.ModuleExecution, contract.PublishedQuery, QueryReplayRequest) (contract.QueryReplayPage, error)
}

// ScopedPublishedQueryReplayExecutor preserves actor filtering in offline feeds.
// Providers must apply this scope before selecting/page-limiting changes.
type ScopedPublishedQueryReplayExecutor interface {
	ReplayPublishedQueryForActor(context.Context, contract.ModuleExecution, contract.PublishedQuery, PublishedQueryActorScope, QueryReplayRequest) (contract.QueryReplayPage, error)
}

// OfflineActorScopeSupport lets a provider fail closed for policy row scopes
// it cannot enforce at its source. Providers without this optional interface
// retain the existing contract and are expected to support every authorized
// actor scope they advertise as offline eligible.
type OfflineActorScopeSupport interface {
	SupportsOfflineActorScope(PublishedQueryActorScope) bool
}

// ResolvedPublishedQuery is a published query bound to the provider the plan
// selected for it.
type ResolvedPublishedQuery struct {
	Definition contract.PublishedQuery
	ProviderID string
	Revision   uint64
	executor   PublishedQueryExecutor
	replay     PublishedQueryReplayExecutor
	validate   func(contract.SchemaRef, []byte) error
}

// CanReplayPublishedQueryOffline reports whether the selected provider opted
// into offline sync and implements actor-scoped replay enforcement.
func (p BindingPlan) CanReplayPublishedQueryOffline(scope contract.BusinessScope, owner contract.ModuleID, name string) bool {
	resolved, ok := p.Queries[PublishedQueryKey{Scope: scope, OwnerModuleID: owner, Name: name}]
	if !ok || !resolved.Definition.OfflineEligible || resolved.replay == nil {
		return false
	}
	_, ok = resolved.replay.(ScopedPublishedQueryReplayExecutor)
	return ok
}

// CanReplayPublishedQueryOfflineForActor additionally asks the provider
// whether it can enforce the given actor scope at its source.
func (p BindingPlan) CanReplayPublishedQueryOfflineForActor(scope contract.BusinessScope, owner contract.ModuleID, name string, actor *PublishedQueryActorScope) bool {
	if !p.CanReplayPublishedQueryOffline(scope, owner, name) || actor == nil {
		return p.CanReplayPublishedQueryOffline(scope, owner, name)
	}
	resolved := p.Queries[PublishedQueryKey{Scope: scope, OwnerModuleID: owner, Name: name}]
	if support, ok := resolved.executor.(OfflineActorScopeSupport); ok {
		return support.SupportsOfflineActorScope(*actor)
	}
	return true
}

// ExecutePublishedQuery reads one page of a published query. A nil actor runs
// the query without a verified actor; otherwise the actor scope narrows rows.
func (p BindingPlan) ExecutePublishedQuery(ctx context.Context, key PublishedQueryKey, actor *PublishedQueryActorScope, page QueryPageRequest) (contract.QueryResult, error) {
	// A scope without assignments names the owner field: OwnerOnly narrows to
	// the actor's rows, otherwise it only carries the verified actor (#407).
	if actor != nil && (actor.BusinessScope != key.Scope || strings.TrimSpace(actor.SubjectID) == "" || len(actor.AssignedFields) == 0 && actor.OwnerField == "") {
		return contract.QueryResult{}, fmt.Errorf("published query actor scope is invalid")
	}
	return p.executePublishedQuery(ctx, key, actor, page)
}
