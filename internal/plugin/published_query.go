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

func (p BindingPlan) executePublishedQuery(ctx context.Context, key PublishedQueryKey, actorScope *PublishedQueryActorScope, page QueryPageRequest) (contract.QueryResult, error) {
	resolved, ok := p.Queries[key]
	if !ok || resolved.executor == nil || resolved.validate == nil {
		return contract.QueryResult{}, fmt.Errorf("published query provider is unavailable")
	}
	query := resolved.Definition
	if key.Scope.Validate() != nil || contract.ValidateResourceBusinessScope(key.Scope, query.ScopePolicy) != nil || len(page.Input) == 0 || len(page.Input) > 65536 || !json.Valid(page.Input) {
		return contract.QueryResult{}, fmt.Errorf("published query request is invalid")
	}
	if page.Limit == 0 {
		page.Limit = query.PageLimit
	}
	if page.Limit > query.PageLimit {
		return contract.QueryResult{}, &QueryInputError{Field: "limit", Message: fmt.Sprintf("limit must not exceed %d for this query", query.PageLimit)}
	}
	if len(page.Cursor) > 2048 {
		return contract.QueryResult{}, &QueryInputError{Field: "cursor", Message: "cursor is invalid"}
	}
	if err := resolved.validate(query.InputSchema, page.Input); err != nil {
		return contract.QueryResult{}, newQueryInputError(err)
	}
	// rowScoped scopes filter rows; a verified actor alone (owner_field query
	// under a plain grant or the Administrator bypass) reads every row.
	rowScoped := actorScope != nil && (actorScope.OwnerOnly || len(actorScope.AssignedFields) > 0)
	if actorScope != nil {
		allowed := make(map[string]bool, len(query.AllowedFields))
		for _, field := range query.AllowedFields {
			allowed[field] = true
		}
		for _, field := range actorScope.AssignedFields {
			if !allowed[field] {
				return contract.QueryResult{}, fmt.Errorf("published query assignment field %q is not published", field)
			}
		}
		if actorScope.OwnerOnly && (!allowed[actorScope.OwnerField] || query.OwnerField != actorScope.OwnerField) {
			return contract.QueryResult{}, fmt.Errorf("published query owner field is not declared")
		}
		if !rowScoped && (query.OwnerField == "" || query.OwnerField != actorScope.OwnerField) {
			return contract.QueryResult{}, fmt.Errorf("published query owner field is not declared")
		}
	}
	execution, err := contract.NewModuleExecution(key.Scope, key.OwnerModuleID, key.OwnerModuleID, resolved.Revision, query.Operation)
	if err != nil {
		return contract.QueryResult{}, err
	}
	var result contract.QueryResult
	scoped, scopedOK := resolved.executor.(ScopedPublishedQueryExecutor)
	if actorScope == nil || !rowScoped && !scopedOK {
		result, err = resolved.executor.ExecutePublishedQuery(ctx, execution, query, QueryPageRequest{Input: append(json.RawMessage(nil), page.Input...), Cursor: page.Cursor, Limit: page.Limit})
	} else {
		if !scopedOK {
			return contract.QueryResult{}, fmt.Errorf("published query provider does not support actor row scopes")
		}
		result, err = scoped.ExecutePublishedQueryForActor(ctx, execution, query, *actorScope, QueryPageRequest{Input: append(json.RawMessage(nil), page.Input...), Cursor: page.Cursor, Limit: page.Limit})
	}
	if err != nil {
		return contract.QueryResult{}, err
	}
	if len(result.Data) > 1<<20 {
		return contract.QueryResult{}, fmt.Errorf("published query result exceeds the response size limit")
	}
	if err := resolved.validate(query.OutputSchema, result.Data); err != nil {
		return contract.QueryResult{}, fmt.Errorf("published query output failed schema validation: %w", err)
	}
	if err := result.Validate(query, page.Limit, key.Scope); err != nil {
		return contract.QueryResult{}, err
	}
	if rowScoped {
		var rows []map[string]json.RawMessage
		if err := json.Unmarshal(result.Data, &rows); err != nil {
			return contract.QueryResult{}, fmt.Errorf("scoped published query returned invalid rows")
		}
		for _, row := range rows {
			matched := false
			if actorScope.OwnerOnly {
				var subject string
				if err := json.Unmarshal(row[actorScope.OwnerField], &subject); err == nil && subject == actorScope.SubjectID {
					matched = true
				}
			}
			for _, field := range actorScope.AssignedFields {
				var subject string
				if err := json.Unmarshal(row[field], &subject); err == nil && subject == actorScope.SubjectID {
					matched = true
					break
				}
			}
			if !matched {
				return contract.QueryResult{}, fmt.Errorf("published query provider returned a row outside actor assignment scope")
			}
		}
	}
	return result, nil
}
