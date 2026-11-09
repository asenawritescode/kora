package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/plugin"
	"github.com/asenawritescode/kora/internal/query"
	"github.com/asenawritescode/kora/internal/site"
)

func configQueryFixture() (*query.Definition, contract.ModuleDescriptor) {
	in := contract.SchemaRef{ID: "cq.input", Version: 1}
	out := contract.SchemaRef{ID: "cq.output", Version: 1}
	def := &query.Definition{Name: "open_items", ModuleOwnerID: "cq.owner", Capability: "cq.read", Operation: "list", Source: "Item", Scope: "tenant", Fields: []string{"name", "title"}, Limit: 2, Sort: []query.Sort{{Field: "name"}}, OfflineEligible: true}
	pq := contract.PublishedQuery{OwnerModuleID: "cq.owner", Name: "list", Capability: "cq.read", Operation: "list", InputSchema: in, OutputSchema: out, ScopePolicy: contract.ScopeTenantShared, AllowedFields: []string{"name", "title"}, PageLimit: 2, OfflineEligible: true, OfflineFactKey: "name"}
	offer := contract.CapabilityOffer{Name: "cq.read", Contract: out, Operations: []contract.OperationSpec{{Name: "list", Input: in, Output: out, Effect: contract.EffectQuery}}}
	return def, contract.ModuleDescriptor{ID: "cq.owner", Version: "1.0.0", Queries: []contract.PublishedQuery{pq}, Provides: []contract.CapabilityOffer{offer}}
}

func TestRegisteredConfigQueryProvidersMatchesDescriptor(t *testing.T) {
	schemas := publishedQueryTestSchemas{{ID: "cq.input", Version: 1}: true, {ID: "cq.output", Version: 1}: true}
	tests := []struct {
		name    string
		mutate  func(*query.Definition, *contract.ModuleDescriptor)
		wantErr string
	}{
		{"match", func(*query.Definition, *contract.ModuleDescriptor) {}, ""},
		{"fields differ", func(d *query.Definition, _ *contract.ModuleDescriptor) { d.Fields = []string{"name"} }, "query.mismatch"},
		{"limit differs", func(d *query.Definition, _ *contract.ModuleDescriptor) { d.Limit = 9 }, "query.mismatch"},
		{"owner field differs", func(d *query.Definition, _ *contract.ModuleDescriptor) { d.OwnerField = "owner" }, "query.mismatch"},
		{"no descriptor", func(d *query.Definition, _ *contract.ModuleDescriptor) { d.Operation = "other" }, "query.unpublished"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def, module := configQueryFixture()
			tt.mutate(def, &module)
			modules := []contract.ModuleDescriptor{module}
			providers, err := registeredConfigQueryProviders(moduleProviderSource{tenant: "t1", dialect: db.Resolve("libsql"), modules: modules, schemas: schemas}, []*query.Definition{def}, true)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %s", err, tt.wantErr)
				}
				return
			}
			if err != nil || len(providers) != 1 || providers[0].ProviderID != "engine.config-queries.cq.owner" || providers[0].Queries["list"] == nil {
				t.Fatalf("providers = %+v err = %v", providers, err)
			}
			database, _ := sql.Open("sqlite3", ":memory:")
			defer database.Close()
			facts, err := registeredSQLPublishedQueryProviders(context.Background(), moduleProviderSource{tenant: "t1", db: database, dialect: db.Resolve("libsql"), modules: modules, schemas: schemas, configured: providers})
			if err != nil || len(facts) != 0 {
				t.Fatalf("fact provider registered for config query: %+v err=%v", facts, err)
			}
		})
	}
}

func TestConfigQueryExecutorPagesWithCursor(t *testing.T) {
	database, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if _, err := database.Exec("CREATE TABLE `tabItem` (name TEXT PRIMARY KEY, title TEXT); INSERT INTO `tabItem` VALUES ('a','A'),('b','B'),('c','C')"); err != nil {
		t.Fatal(err)
	}
	def, _ := configQueryFixture()
	exec := &configQueryExecutor{db: database, dialect: db.Resolve("libsql"), def: def}
	scope := plugin.PublishedQueryActorScope{SubjectID: "u1"}
	var names []string
	cursor := ""
	for page := 0; page < 3; page++ {
		res, err := exec.ExecutePublishedQueryForActor(context.Background(), contract.ModuleExecution{}, contract.PublishedQuery{}, scope, json.RawMessage(`{}`), cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		var rows []map[string]any
		if err := json.Unmarshal(res.Data, &rows); err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			names = append(names, r["name"].(string))
		}
		if cursor = res.NextCursor; cursor == "" {
			break
		}
	}
	if strings.Join(names, ",") != "a,b,c" {
		t.Fatalf("names = %v", names)
	}
	own := *def
	own.OwnerField = "title"
	if _, err := (&configQueryExecutor{db: database, dialect: db.Resolve("libsql"), def: &own}).ExecutePublishedQuery(context.Background(), contract.ModuleExecution{}, contract.PublishedQuery{}, json.RawMessage(`{}`), "", 2); err == nil {
		t.Fatal("owner-restricted query ran without actor scope")
	}
}

func TestConfigQueryProvidersChildSourceUsesParent(t *testing.T) {
	schemas := publishedQueryTestSchemas{{ID: "cq.input", Version: 1}: true, {ID: "cq.output", Version: 1}: true}
	for _, scopeField := range []string{"", "entity"} {
		reg := doctype.NewRegistry()
		reg.Register(&doctype.DocType{Name: "Sale", BusinessScopeField: scopeField})
		def, module := configQueryFixture()
		def.Via, def.Scope = "Sale.payments", "legal_entity"
		module.Queries[0].ScopePolicy = contract.ScopeEntityOwned
		_, err := registeredConfigQueryProviders(moduleProviderSource{tenant: "t1", dialect: db.Resolve("libsql"), registry: reg, modules: []contract.ModuleDescriptor{module}, schemas: schemas}, []*query.Definition{def}, true)
		src, _ := reg.QuerySource("module.cq.owner.query.list")
		if (scopeField == "") != (err != nil) || (err == nil && src != "Sale") {
			t.Fatalf("scope %q: src=%q err=%v", scopeField, src, err)
		}
	}
}

func TestRegisteredConfigQueryProvidersLenientSkips(t *testing.T) {
	schemas := publishedQueryTestSchemas{{ID: "cq.input", Version: 1}: true, {ID: "cq.output", Version: 1}: true}
	tests := []struct {
		name   string
		mutate func(*query.Definition)
		code   string
	}{
		{"unpublished", func(d *query.Definition) { d.Operation = "other" }, "query.unpublished"},
		{"mismatch", func(d *query.Definition) { d.Limit = 9 }, "query.mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad, module := configQueryFixture()
			bad.Name = "bad"
			tt.mutate(bad)
			good, _ := configQueryFixture()
			modules := []contract.ModuleDescriptor{module}
			defs := []*query.Definition{bad, good}
			if _, err := registeredConfigQueryProviders(moduleProviderSource{tenant: "t1", dialect: db.Resolve("libsql"), modules: modules, schemas: schemas}, defs, true); err == nil || !strings.Contains(err.Error(), tt.code) {
				t.Fatalf("strict err = %v, want %s", err, tt.code)
			}
			providers, err := registeredConfigQueryProviders(moduleProviderSource{tenant: "t1", dialect: db.Resolve("libsql"), modules: modules, schemas: schemas}, defs, false)
			if err != nil || len(providers) != 1 || len(providers[0].Queries) != 1 || providers[0].Queries["list"] == nil {
				t.Fatalf("lenient providers = %+v err = %v", providers, err)
			}
			none, err := registeredConfigQueryProviders(moduleProviderSource{tenant: "t1", dialect: db.Resolve("libsql"), schemas: schemas}, defs[:1], false)
			if err != nil || len(none) != 0 {
				t.Fatalf("no modules: %+v err=%v", none, err)
			}
		})
	}
}

func TestGeneratedPackModulesRegisterEveryQuery(t *testing.T) {
	defs := []*query.Definition{
		{Name: "recent", ModuleOwnerID: "shop.core", Capability: "shop.sales", Operation: "recent", Source: "Sale", Scope: "tenant", Fields: []string{"name", "total"}, Limit: 20},
		{Name: "by_method", ModuleOwnerID: "shop.core", Capability: "shop.sales", Operation: "by_method", Source: "Sale", Scope: "tenant", GroupBy: []string{"method"}, Aggregates: []query.Aggregate{{Name: "total", Fn: "sum", Field: "amount"}}, Limit: 50},
	}
	snap := &doctype.ConfigSnapshot{}
	for _, d := range defs {
		raw, _ := json.Marshal(d)
		snap.Queries = append(snap.Queries, raw)
	}
	modules, err := site.GeneratePackModules(contract.PackManifest{Name: "shop", Version: "1"}, snap)
	if err != nil {
		t.Fatal(err)
	}
	schemas := publishedQueryTestSchemas{}
	for _, m := range modules {
		for _, s := range m.Schemas {
			schemas[s.Ref] = true
		}
	}
	providers, err := registeredConfigQueryProviders(moduleProviderSource{tenant: "t1", dialect: db.Resolve("libsql"), modules: modules, schemas: schemas}, defs, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || len(providers[0].Queries) != 2 {
		t.Fatalf("registered %+v", providers)
	}
}

// TestConfigQueryExecutorReportsCallerInputErrors keeps a caller's mistake
// distinguishable from a provider fault (#317).
func TestConfigQueryExecutorReportsCallerInputErrors(t *testing.T) {
	database, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if _, err := database.Exec("CREATE TABLE `tabItem` (name TEXT PRIMARY KEY, title TEXT)"); err != nil {
		t.Fatal(err)
	}
	base, _ := configQueryFixture()
	base.Input = map[string]query.InputSpec{"title": {Type: "string"}}
	base.Filters = []query.Filter{{Field: "title", Op: "eq", Value: "$input.title"}}
	tests := []struct {
		name      string
		input     string
		cursor    string
		wantField string
	}{
		{"missing required input", `{}`, "", "title"},
		{"input not an object", `[]`, "", ""},
		{"malformed cursor", `{"title":"x"}`, "!!", "cursor"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exec := &configQueryExecutor{db: database, dialect: db.Resolve("libsql"), def: base}
			_, err := exec.ExecutePublishedQueryForActor(context.Background(), contract.ModuleExecution{}, contract.PublishedQuery{}, plugin.PublishedQueryActorScope{SubjectID: "u1"}, json.RawMessage(tc.input), tc.cursor, 2)
			var inputErr *plugin.QueryInputError
			if !errors.As(err, &inputErr) || inputErr.Field != tc.wantField {
				t.Fatalf("err = %#v, want *plugin.QueryInputError field %q", err, tc.wantField)
			}
		})
	}
}

// A caller limit below page_limit must size the page itself: the binding layer
// rejects a result with more rows than the requested limit, which surfaced as
// 503 module.query_failed (#335).
func TestConfigQueryExecutorHonoursLimitBelowPageLimit(t *testing.T) {
	database, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if _, err := database.Exec("CREATE TABLE `tabItem` (name TEXT PRIMARY KEY, title TEXT); INSERT INTO `tabItem` VALUES ('a','A'),('b','B'),('c','C'),('d','D'),('e','E')"); err != nil {
		t.Fatal(err)
	}
	def, module := configQueryFixture()
	def.Limit = 4
	pq := module.Queries[0]
	pq.PageLimit = 4
	scope := contract.BusinessScope{TenantID: "t1", Kind: contract.ScopeKindTenant}
	exec := &configQueryExecutor{db: database, dialect: db.Resolve("libsql"), def: def}
	tests := []struct {
		name      string
		limit     uint16
		wantPages []string
	}{
		{"limit 1", 1, []string{"a", "b", "c", "d", "e"}},
		{"page_limit-1", 3, []string{"abc", "de"}},
		{"page_limit", 4, []string{"abcd", "e"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			cursor := ""
			for range 10 {
				res, err := exec.ExecutePublishedQueryForActor(context.Background(), contract.ModuleExecution{}, pq, plugin.PublishedQueryActorScope{SubjectID: "u1"}, json.RawMessage(`{}`), cursor, tt.limit)
				if err != nil {
					t.Fatal(err)
				}
				if err := res.Validate(pq, tt.limit, scope); err != nil {
					t.Fatalf("result violates the page contract for limit %d: %v", tt.limit, err)
				}
				var rows []map[string]any
				if err := json.Unmarshal(res.Data, &rows); err != nil {
					t.Fatal(err)
				}
				page := ""
				for _, r := range rows {
					page += r["name"].(string)
				}
				got = append(got, page)
				if cursor = res.NextCursor; cursor == "" {
					break
				}
			}
			if strings.Join(got, ",") != strings.Join(tt.wantPages, ",") {
				t.Fatalf("pages = %v, want %v", got, tt.wantPages)
			}
		})
	}
	// A limit above page_limit is the caller's mistake, not a provider fault.
	_, err = exec.ExecutePublishedQueryForActor(context.Background(), contract.ModuleExecution{}, pq, plugin.PublishedQueryActorScope{SubjectID: "u1"}, json.RawMessage(`{}`), "", 5)
	var inputErr *plugin.QueryInputError
	if !errors.As(err, &inputErr) || inputErr.Field != "limit" {
		t.Fatalf("limit above page_limit: err = %v, want limit QueryInputError", err)
	}
}

func TestConfigQueryRunRejectsMalformedInput(t *testing.T) {
	tests := []struct {
		name  string
		call  configQueryCall
		field string
	}{
		{"input is not an object", configQueryCall{input: json.RawMessage(`[]`), full: true}, ""},
		{"cursor is not decodable", configQueryCall{input: json.RawMessage(`{}`), cursor: "!!", full: true}, "cursor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := (&configQueryExecutor{def: &query.Definition{}}).run(context.Background(), tt.call)
			var inputErr *plugin.QueryInputError
			if !errors.As(err, &inputErr) || inputErr.Field != tt.field {
				t.Fatalf("run error = %v, want QueryInputError on field %q", err, tt.field)
			}
		})
	}
}
