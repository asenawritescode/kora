package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/plugin"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/query"
	"github.com/gin-gonic/gin"
)

// actorScopeFixture serves POS-shaped config queries over Till Session rows
// through the real gate, binding plan and config executor (#407).
func actorScopeFixture(t *testing.T) (*doctype.Registry, *api.Handler, map[string]*configQueryExecutor) {
	t.Helper()
	database, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	database.SetMaxOpenConns(1)
	if _, err := database.Exec("CREATE TABLE `tabTill Session` (name TEXT PRIMARY KEY, cashier TEXT, status TEXT); INSERT INTO `tabTill Session` VALUES ('TS-1','cashier-1','Open'),('TS-2','cashier-2','Open'),('TS-3','admin-1','Open'),('TS-4','cashier-1','Closed'),('TS-5','cashier-2','Closed')"); err != nil {
		t.Fatal(err)
	}
	in, out := contract.SchemaRef{ID: "pos.till.input", Version: 1}, contract.SchemaRef{ID: "pos.till.output", Version: 1}
	fields := []string{"name", "cashier"}
	byName := []query.Sort{{Field: "name"}}
	defs := []*query.Definition{
		{Name: "my_session", OwnerField: "cashier", Filters: []query.Filter{{Field: "cashier", Op: "eq", Value: "$actor"}, {Field: "status", Op: "eq", Value: "Open"}}},
		{Name: "my_closed_session", OwnerField: "cashier", Input: map[string]query.InputSpec{"name": {Type: "string"}}, Filters: []query.Filter{{Field: "name", Op: "eq", Value: "$input.name"}, {Field: "status", Op: "eq", Value: "Closed"}}},
		{Name: "sessions", OwnerField: "cashier"},
		{Name: "open_till", Filters: []query.Filter{{Field: "status", Op: "eq", Value: "Open"}}},
	}
	module := contract.ModuleDescriptor{ID: "pos.core", Version: "1.0.0"}
	offer := contract.CapabilityOffer{Name: "pos.till", Contract: out}
	for _, def := range defs {
		def.ModuleOwnerID, def.Capability, def.Operation, def.Source, def.Scope, def.Fields, def.Sort, def.Limit = "pos.core", "pos.till", def.Name, "Till Session", "tenant", fields, byName, 10
		module.Queries = append(module.Queries, contract.PublishedQuery{OwnerModuleID: "pos.core", Name: def.Name, Capability: "pos.till", Operation: def.Name, InputSchema: in, OutputSchema: out, ScopePolicy: contract.ScopeTenantShared, AllowedFields: fields, PageLimit: 10, OwnerField: def.OwnerField})
		offer.Operations = append(offer.Operations, contract.OperationSpec{Name: def.Name, Input: in, Output: out, Effect: contract.EffectQuery})
	}
	module.Provides = []contract.CapabilityOffer{offer}
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Till Session", ModuleOwnerID: "pos.core"})
	providers, err := registeredConfigQueryProviders("pos-t", database, db.Resolve("libsql"), registry, []contract.ModuleDescriptor{module}, defs, publishedQueryTestSchemas{in: true, out: true}, true)
	if err != nil || len(providers) != 1 {
		t.Fatalf("providers = %+v err = %v", providers, err)
	}
	grant := func(role, q string, ifOwner bool) {
		registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.pos.core.query." + q, Role: role, Read: true, IfOwner: ifOwner})
	}
	grant("Cashier", "my_session", true)
	grant("Cashier", "my_closed_session", true)
	grant("Cashier", "sessions", true)
	grant("Cashier", "open_till", false)
	grant("Manager", "my_session", false)
	grant("Manager", "my_closed_session", false)
	grant("Manager", "sessions", false)
	grant("Manager", "open_till", false)
	grant("Owner", "open_till", false)
	scope := contract.BusinessScope{TenantID: "pos-t", Kind: contract.ScopeKindTenant}
	selection := plugin.ProviderSelection{Key: plugin.BindingKey{Scope: scope, ConsumerModuleID: module.ID, CapabilityName: "pos.till"}, ProviderID: providers[0].ProviderID}
	services := api.NewSiteRuntimeServices()
	services.Replace("pos-t", api.SiteRuntimeService{ResolveModuleBindings: func(requested contract.BusinessScope) (plugin.BindingPlan, error) {
		return plugin.ResolveModules(requested, []contract.ModuleDescriptor{module}, providers, []plugin.ProviderSelection{selection}, 1)
	}})
	handler := api.NewHandler(registry, nil)
	handler.RuntimeServices = services
	executors := map[string]*configQueryExecutor{}
	for name, e := range providers[0].Queries {
		executors[name] = e.(*configQueryExecutor)
	}
	return registry, handler, executors
}

func servePOSQuery(t *testing.T, handler *api.Handler, role, subject, name, body string) (int, []string) {
	t.Helper()
	execution, err := policy.NewVerifiedExecution("pos-t", contract.ActorContext{PrincipalID: subject, SubjectUserID: subject, PrincipalType: contract.PrincipalHuman, Site: "pos-t", Roles: []string{role}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/modules/pos.core/queries/"+name, strings.NewReader(body))
	ctx.Params = gin.Params{{Key: "module", Value: "pos.core"}, {Key: "query", Value: name}}
	ctx.Set("site_name", "pos-t")
	ctx.Set("policy_execution", execution)
	handler.HandlePublishedModuleQuery(ctx)
	if recorder.Code != http.StatusOK {
		return recorder.Code, nil
	}
	var out struct {
		Data struct {
			Data []map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %s: %v", recorder.Body.String(), err)
	}
	names := []string{}
	for _, row := range out.Data.Data {
		names = append(names, row["name"].(string))
	}
	return recorder.Code, names
}

// TestPublishedQueryActorScopeByGrant is the #407 access matrix: an
// owner_field query always runs with the verified actor; only an if_owner
// grant narrows it to the actor's rows, and $actor is never request input.
func TestPublishedQueryActorScopeByGrant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, handler, _ := actorScopeFixture(t)
	closed := func(name string) string { return `{"input":{"name":"` + name + `"}}` }
	tests := []struct {
		name, role, subject, query, body string
		wantStatus                       int
		wantRows                         []string
	}{
		{"cashier if_owner my_session sees own open till", "Cashier", "cashier-1", "my_session", `{"input":{}}`, 200, []string{"TS-1"}},
		{"cashier if_owner my_closed_session own", "Cashier", "cashier-1", "my_closed_session", closed("TS-4"), 200, []string{"TS-4"}},
		{"cashier if_owner my_closed_session other cashier", "Cashier", "cashier-1", "my_closed_session", closed("TS-5"), 200, []string{}},
		{"cashier if_owner owner_field query without $actor", "Cashier", "cashier-1", "sessions", `{"input":{}}`, 200, []string{"TS-1", "TS-4"}},
		{"cashier plain grant on non-owner query", "Cashier", "cashier-1", "open_till", `{"input":{}}`, 200, []string{"TS-1", "TS-2", "TS-3"}},
		{"manager plain my_session binds $actor", "Manager", "manager-1", "my_session", `{"input":{}}`, 200, []string{}},
		{"manager plain my_closed_session any till", "Manager", "manager-1", "my_closed_session", closed("TS-5"), 200, []string{"TS-5"}},
		{"manager plain owner_field query all rows", "Manager", "manager-1", "sessions", `{"input":{}}`, 200, []string{"TS-1", "TS-2", "TS-3", "TS-4", "TS-5"}},
		{"owner plain grant on non-owner query unchanged", "Owner", "owner-1", "open_till", `{"input":{}}`, 200, []string{"TS-1", "TS-2", "TS-3"}},
		{"owner without my_session grant denied", "Owner", "owner-1", "my_session", `{"input":{}}`, 403, nil},
		{"administrator my_session returns own open till", doctype.AdminRole, "admin-1", "my_session", `{"input":{}}`, 200, []string{"TS-3"}},
		{"administrator my_closed_session any till", doctype.AdminRole, "admin-1", "my_closed_session", closed("TS-4"), 200, []string{"TS-4"}},
		{"administrator owner_field query all rows", doctype.AdminRole, "admin-1", "sessions", `{"input":{}}`, 200, []string{"TS-1", "TS-2", "TS-3", "TS-4", "TS-5"}},
		{"administrator non-owner query unchanged", doctype.AdminRole, "admin-1", "open_till", `{"input":{}}`, 200, []string{"TS-1", "TS-2", "TS-3"}},
		{"no grant denied my_session", "Nobody", "nobody-1", "my_session", `{"input":{}}`, 403, nil},
		{"no grant denied sessions", "Nobody", "nobody-1", "sessions", `{"input":{}}`, 403, nil},
		{"no grant denied open_till", "Nobody", "nobody-1", "open_till", `{"input":{}}`, 403, nil},
		// Spoofing: the actor comes only from the verified execution.
		{"cashier input cashier ignored", "Cashier", "cashier-1", "my_session", `{"input":{"cashier":"cashier-2"}}`, 200, []string{"TS-1"}},
		{"cashier input actor ignored", "Cashier", "cashier-1", "my_session", `{"input":{"actor":"admin-1","$actor":"admin-1"}}`, 200, []string{"TS-1"}},
		{"administrator input cashier ignored", doctype.AdminRole, "admin-1", "my_session", `{"input":{"cashier":"cashier-1"}}`, 200, []string{"TS-3"}},
		{"top-level actor field rejected", "Cashier", "cashier-1", "my_session", `{"input":{},"actor":"admin-1"}`, 400, nil},
		{"cashier input owner field on owner_field query ignored", "Cashier", "cashier-1", "sessions", `{"input":{"cashier":"cashier-2"}}`, 200, []string{"TS-1", "TS-4"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, rows := servePOSQuery(t, handler, tc.role, tc.subject, tc.query, tc.body)
			if status != tc.wantStatus || tc.wantRows != nil && !slices.Equal(rows, tc.wantRows) {
				t.Fatalf("%s %s = %d %v, want %d %v", tc.role, tc.query, status, rows, tc.wantStatus, tc.wantRows)
			}
		})
	}
}

// TestConfigQueryExecutorActorScope pins the executor's own fail-closed rules
// for owner_field queries below the gate.
func TestConfigQueryExecutorActorScope(t *testing.T) {
	_, _, executors := actorScopeFixture(t)
	scope := contract.BusinessScope{TenantID: "pos-t", Kind: contract.ScopeKindTenant}
	tests := []struct {
		name    string
		query   string
		actor   *plugin.PublishedQueryActorScope
		want    []string
		wantErr bool
	}{
		{"no actor on owner_field query", "my_session", nil, nil, true},
		{"no actor on non-owner query", "open_till", nil, []string{"TS-1", "TS-2", "TS-3"}, false},
		{"empty subject on owner_field query", "sessions", &plugin.PublishedQueryActorScope{BusinessScope: scope, OwnerField: "cashier"}, nil, true},
		{"owner only filters to subject", "sessions", &plugin.PublishedQueryActorScope{BusinessScope: scope, SubjectID: "cashier-2", OwnerOnly: true, OwnerField: "cashier"}, []string{"TS-2", "TS-5"}, false},
		{"verified full read sees all", "sessions", &plugin.PublishedQueryActorScope{BusinessScope: scope, SubjectID: "admin-1", OwnerField: "cashier"}, []string{"TS-1", "TS-2", "TS-3", "TS-4", "TS-5"}, false},
		{"verified full read binds $actor", "my_session", &plugin.PublishedQueryActorScope{BusinessScope: scope, SubjectID: "admin-1", OwnerField: "cashier"}, []string{"TS-3"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := executors[tc.query]
			var res contract.QueryResult
			var err error
			if tc.actor == nil {
				res, err = e.ExecutePublishedQuery(context.Background(), contract.ModuleExecution{}, contract.PublishedQuery{}, json.RawMessage(`{}`), "", 10)
			} else {
				res, err = e.ExecutePublishedQueryForActor(context.Background(), contract.ModuleExecution{}, contract.PublishedQuery{}, *tc.actor, json.RawMessage(`{}`), "", 10)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			var rows []map[string]any
			if err := json.Unmarshal(res.Data, &rows); err != nil {
				t.Fatal(err)
			}
			names := []string{}
			for _, r := range rows {
				names = append(names, r["name"].(string))
			}
			if !slices.Equal(names, tc.want) {
				t.Fatalf("rows = %v, want %v", names, tc.want)
			}
		})
	}
}
