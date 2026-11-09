package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/query"
	"github.com/asenawritescode/kora/workflow"
)

// registeredConfigQueryProviders binds each tenant config query to the module
// descriptor query it executes. The descriptor stays the published contract;
// the config definition supplies only execution. A definition without a
// descriptor, or one that disagrees with it, is an error when strict (preflight)
// and is skipped with a warning otherwise (tenant start), so the site still starts.
func registeredConfigQueryProviders(tenant contract.TenantID, database *sql.DB, dialect db.Dialect, registry *doctype.Registry, modules []contract.ModuleDescriptor, defs []*query.Definition, schemas workflow.PayloadSchemaRegistry, strict bool) ([]plugin.ProviderRegistration, error) {
	byOwner := map[contract.ModuleID]map[string]plugin.PublishedQueryExecutor{}
	offers := map[contract.ModuleID][]contract.CapabilityOffer{}
	for _, def := range defs {
		owner := contract.ModuleID(def.ModuleOwnerID)
		var module *contract.ModuleDescriptor
		var published *contract.PublishedQuery
		for i := range modules {
			if modules[i].ID != owner {
				continue
			}
			module = &modules[i]
			for j := range module.Queries {
				q := &module.Queries[j]
				if q.Name == def.Operation && string(q.Capability) == def.Capability {
					published = q
				}
			}
		}
		if published == nil {
			err := fmt.Errorf("query.unpublished: config query %q has no module descriptor query %s/%s", def.Name, def.ModuleOwnerID, def.Operation)
			if strict {
				return nil, err
			}
			slog.Warn("skipping config query", "tenant", tenant, "query", def.Name, "code", "query.unpublished", "error", err)
			continue
		}
		if err := matchConfigQuery(def, *published); err != nil {
			if strict {
				return nil, err
			}
			slog.Warn("skipping config query", "tenant", tenant, "query", def.Name, "code", "query.mismatch", "error", err)
			continue
		}
		if schemas == nil || !schemas.Registered(published.InputSchema) || !schemas.Registered(published.OutputSchema) {
			return nil, fmt.Errorf("config query %q requires registered input and output schemas", def.Name)
		}
		if byOwner[owner] == nil {
			byOwner[owner] = map[string]plugin.PublishedQueryExecutor{}
		}
		scopeField := ""
		permSource := def.Source
		if p := def.ParentDocType(); p != "" {
			permSource = p
		}
		if def.Scope == "legal_entity" {
			if dt := registry.Get(permSource); dt != nil {
				scopeField = dt.BusinessScopeField
			}
			if scopeField == "" {
				return nil, fmt.Errorf("config query %q is legal_entity scoped but %q has no business_scope_field", def.Name, permSource)
			}
		}
		if registry != nil {
			registry.SetQuerySource("module."+def.ModuleOwnerID+".query."+published.Name, permSource)
		}
		byOwner[owner][published.Name] = &configQueryExecutor{db: database, dialect: dialect, def: def, scopeField: scopeField}
		for _, offer := range module.Provides {
			if offer.Name == published.Capability && !slices.ContainsFunc(offers[owner], func(o contract.CapabilityOffer) bool { return o.Name == offer.Name }) {
				offers[owner] = append(offers[owner], offer)
			}
		}
	}
	owners := make([]string, 0, len(byOwner))
	for owner := range byOwner {
		owners = append(owners, string(owner))
	}
	sort.Strings(owners)
	var out []plugin.ProviderRegistration
	for _, o := range owners {
		owner := contract.ModuleID(o)
		out = append(out, plugin.ProviderRegistration{
			TenantID: tenant, ProviderID: "engine.config-queries." + o, OwnerModuleID: owner, Kind: plugin.ProviderInternal,
			Offers: offers[owner], Queries: byOwner[owner],
			ValidateSchema: func(ref contract.SchemaRef, raw []byte) error {
				if !schemas.Registered(ref) {
					return fmt.Errorf("schema %s@%d is not registered", ref.ID, ref.Version)
				}
				return schemas.Validate(ref, json.RawMessage(raw))
			},
		})
	}
	return out, nil
}

func matchConfigQuery(def *query.Definition, q contract.PublishedQuery) error {
	wantScope := contract.ScopeTenantShared
	if def.Scope == "legal_entity" {
		wantScope = contract.ScopeEntityOwned
	}
	switch {
	case !slices.Equal(q.AllowedFields, def.OutputFields()):
		return fmt.Errorf("query.mismatch: config query %q fields differ from descriptor allowed_fields", def.Name)
	case int(q.PageLimit) != def.Limit:
		return fmt.Errorf("query.mismatch: config query %q limit %d differs from descriptor page_limit %d", def.Name, def.Limit, q.PageLimit)
	case q.OwnerField != def.OwnerField:
		return fmt.Errorf("query.mismatch: config query %q owner_field differs from descriptor", def.Name)
	case q.ScopePolicy != wantScope:
		return fmt.Errorf("query.mismatch: config query %q scope differs from descriptor scope_policy", def.Name)
	}
	return nil
}

// configQueryExecutor runs a config query for an actor whose row scope was
// derived by the binding layer from the verified source-DocType permission.
type configQueryExecutor struct {
	db         *sql.DB
	dialect    db.Dialect
	def        *query.Definition
	scopeField string
}

// ExecutePublishedQuery has no verified actor, so owner-restricted queries fail closed.
func (e *configQueryExecutor) ExecutePublishedQuery(ctx context.Context, exec contract.ModuleExecution, q contract.PublishedQuery, input json.RawMessage, cursor string, limit uint16) (contract.QueryResult, error) {
	if e.def.OwnerField != "" {
		return contract.QueryResult{}, contract.NewError(contract.CodePermissionDenied, "actor scope is required")
	}
	return e.run(ctx, exec.Scope, plugin.PublishedQueryActorScope{}, true, input, cursor, limit)
}

// ExecutePublishedQueryForActor binds $actor to the verified subject. OwnerOnly
// (an if_owner grant) also applies owner_field; otherwise every row is read.
func (e *configQueryExecutor) ExecutePublishedQueryForActor(ctx context.Context, exec contract.ModuleExecution, q contract.PublishedQuery, actor plugin.PublishedQueryActorScope, input json.RawMessage, cursor string, limit uint16) (contract.QueryResult, error) {
	if e.def.OwnerField != "" && strings.TrimSpace(actor.SubjectID) == "" {
		return contract.QueryResult{}, contract.NewError(contract.CodePermissionDenied, "actor scope is required")
	}
	return e.run(ctx, exec.Scope, actor, !actor.OwnerOnly, input, cursor, limit)
}

func (e *configQueryExecutor) run(ctx context.Context, scope contract.BusinessScope, actor plugin.PublishedQueryActorScope, full bool, input json.RawMessage, cursor string, limit uint16) (contract.QueryResult, error) {
	req := query.Request{Limit: int(limit), Actor: actor.SubjectID, Now: time.Now().UTC(), FullRead: full, ScopeField: e.scopeField}
	if e.scopeField != "" {
		req.ScopeValue = scope.LegalEntityID
	}
	if err := json.Unmarshal(input, &req.Input); err != nil {
		return contract.QueryResult{}, &plugin.QueryInputError{Message: "query input must be an object"}
	}
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err != nil || dec.Decode(&req.Cursor) != nil {
			return contract.QueryResult{}, &plugin.QueryInputError{Field: "cursor", Message: "query cursor is invalid"}
		}
	}
	page, err := query.Run(ctx, e.db, e.dialect, e.def, req)
	var inputErr *query.InputError
	if errors.As(err, &inputErr) {
		return contract.QueryResult{}, &plugin.QueryInputError{Field: inputErr.Field, Message: inputErr.Msg}
	}
	if err != nil {
		return contract.QueryResult{}, err
	}
	rows := page.Rows
	if rows == nil {
		rows = []map[string]any{}
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return contract.QueryResult{}, err
	}
	next := ""
	if len(page.Next) > 0 {
		raw, err := json.Marshal(page.Next)
		if err != nil {
			return contract.QueryResult{}, err
		}
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	return contract.QueryResult{Data: data, NextCursor: next, AsOf: req.Now, Freshness: contract.QueryFresh}, nil
}

// configQueriesForModules keeps definitions owned by the candidate modules.
func configQueriesForModules(defs []*query.Definition, modules []contract.ModuleDescriptor) []*query.Definition {
	var out []*query.Definition
	for _, def := range defs {
		if slices.ContainsFunc(modules, func(m contract.ModuleDescriptor) bool { return string(m.ID) == def.ModuleOwnerID }) {
			out = append(out, def)
		}
	}
	return out
}
