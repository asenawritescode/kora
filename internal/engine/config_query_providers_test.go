package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/query"
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
			providers, err := registeredConfigQueryProviders("t1", nil, db.Resolve("libsql"), nil, modules, []*query.Definition{def}, schemas)
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
			facts, err := registeredSQLPublishedQueryProviders(context.Background(), "t1", database, db.Resolve("libsql"), modules, schemas, providers)
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
