package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
)

// ReplayPublishedQueryForActor returns only the authenticated actor's published
// changes. Providers without a source-level scoped replay implementation are
// ineligible for offline feeds.
func (p BindingPlan) ReplayPublishedQueryForActor(ctx context.Context, key PublishedQueryKey, actorScope PublishedQueryActorScope, req QueryReplayRequest) (contract.QueryReplayPage, error) {
	resolved, ok := p.Queries[key]
	if !ok || resolved.replay == nil || resolved.validate == nil || !resolved.Definition.OfflineEligible || actorScope.BusinessScope != key.Scope || strings.TrimSpace(actorScope.SubjectID) == "" {
		return contract.QueryReplayPage{}, fmt.Errorf("published query is not eligible for offline actor replay")
	}
	query := resolved.Definition
	if key.Scope.Validate() != nil || contract.ValidateResourceBusinessScope(key.Scope, query.ScopePolicy) != nil || len(req.Input) == 0 || len(req.Input) > 65536 || !json.Valid(req.Input) || strings.TrimSpace(req.Snapshot) == "" || len(req.Snapshot) > 2048 {
		return contract.QueryReplayPage{}, fmt.Errorf("published query replay identity is invalid")
	}
	if req.Limit == 0 {
		req.Limit = query.PageLimit
	}
	if req.Limit > query.PageLimit || len(req.After) > 2048 || len(req.Through) > 2048 || len(req.Replay) > 2048 {
		return contract.QueryReplayPage{}, fmt.Errorf("published query replay page is outside its registered bounds")
	}
	if err := resolved.validate(query.InputSchema, req.Input); err != nil {
		return contract.QueryReplayPage{}, fmt.Errorf("published query replay input failed schema validation")
	}
	provider, ok := resolved.replay.(ScopedPublishedQueryReplayExecutor)
	if !ok {
		return contract.QueryReplayPage{}, fmt.Errorf("published query provider does not support actor-scoped replay")
	}
	execution, err := contract.NewModuleExecution(key.Scope, key.OwnerModuleID, key.OwnerModuleID, resolved.Revision, query.Operation)
	if err != nil {
		return contract.QueryReplayPage{}, err
	}
	req.Input = append(json.RawMessage(nil), req.Input...)
	page, err := provider.ReplayPublishedQueryForActor(ctx, execution, query, actorScope, req)
	if err != nil {
		return contract.QueryReplayPage{}, err
	}
	if err := page.Validate(query, req.Limit); err != nil {
		return contract.QueryReplayPage{}, err
	}
	if query.OfflineEligible {
		for _, change := range page.Changes {
			if change.Deleted {
				continue
			}
			var row map[string]json.RawMessage
			if err := json.Unmarshal(change.Data, &row); err != nil {
				return contract.QueryReplayPage{}, fmt.Errorf("scoped replay returned an invalid row")
			}
			factKey, err := contract.OfflineQueryFactKey(row, query.OfflineFactKey)
			if err != nil || factKey != change.FactKey {
				return contract.QueryReplayPage{}, fmt.Errorf("scoped replay row does not match its fact key")
			}
		}
	}
	for _, change := range page.Changes {
		if change.Deleted {
			continue
		}
		var row map[string]json.RawMessage
		if err := json.Unmarshal(change.Data, &row); err != nil {
			return contract.QueryReplayPage{}, fmt.Errorf("scoped replay returned an invalid row")
		}
		matched := false
		if actorScope.OwnerOnly {
			var subject string
			_ = json.Unmarshal(row[actorScope.OwnerField], &subject)
			matched = subject == actorScope.SubjectID
		}
		for _, field := range actorScope.AssignedFields {
			var subject string
			_ = json.Unmarshal(row[field], &subject)
			matched = matched || subject == actorScope.SubjectID
		}
		if !matched {
			return contract.QueryReplayPage{}, fmt.Errorf("scoped replay returned a row outside actor scope")
		}
	}
	return page, nil
}
