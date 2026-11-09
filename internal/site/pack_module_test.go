package site

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/query"
)

func packQueries(t *testing.T, defs ...query.Definition) *doctype.ConfigSnapshot {
	t.Helper()
	snap := &doctype.ConfigSnapshot{}
	for _, d := range defs {
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		snap.Queries = append(snap.Queries, raw)
	}
	return snap
}

func TestGeneratePackModules(t *testing.T) {
	plain := query.Definition{Name: "recent", ModuleOwnerID: "shop.core", Capability: "shop.sales", Operation: "recent", Source: "Sale", Scope: "tenant", Fields: []string{"name", "total"}, Limit: 20,
		Input: map[string]query.InputSpec{"register": {Type: "string", MaxLength: 40}, "state": {Type: "string", Enum: []string{"a", "b"}}}, Presentation: map[string]string{"title": "Recent"}}
	agg := query.Definition{Name: "by_method", ModuleOwnerID: "shop.core", Capability: "shop.sales", Operation: "by_method", Source: "Sale Payment", Via: "Sale.payments", Scope: "tenant", GroupBy: []string{"method"}, Aggregates: []query.Aggregate{{Name: "total", Fn: "sum", Field: "amount"}}}
	offline := query.Definition{Name: "items", ModuleOwnerID: "shop.core", Capability: "shop.items", Operation: "items", Source: "Item", Scope: "legal_entity", OwnerField: "owner", Fields: []string{"name", "owner"}, OfflineEligible: true}
	badOwner := plain
	badOwner.ModuleOwnerID = "shop"
	badName := plain
	badName.Name = "other"
	tests := []struct {
		name, version string
		defs          []query.Definition
		wantErr       string
		check         func(*testing.T, []contract.ModuleDescriptor)
	}{
		{"plain", "1", []query.Definition{plain}, "", func(t *testing.T, m []contract.ModuleDescriptor) {
			q := m[0].Queries[0]
			if m[0].Version != "1.0.0" || q.PageLimit != 20 || q.Presentation.Title != "Recent" || q.OfflineEligible {
				t.Fatalf("unexpected %+v", q)
			}
			if !strings.Contains(string(m[0].Schemas[0].Schema), `"additionalProperties":false`) || !strings.Contains(string(m[0].Schemas[0].Schema), `"enum":["a","b"]`) {
				t.Fatalf("input schema %s", m[0].Schemas[0].Schema)
			}
		}},
		{"aggregate", "2.1", []query.Definition{agg}, "", func(t *testing.T, m []contract.ModuleDescriptor) {
			q := m[0].Queries[0]
			if strings.Join(q.AllowedFields, ",") != "method,total" || q.PageLimit != 50 || m[0].Version != "2.1.0" {
				t.Fatalf("unexpected %+v", q)
			}
		}},
		{"offline", "1.2.3", []query.Definition{plain, offline}, "", func(t *testing.T, m []contract.ModuleDescriptor) {
			if !m[0].Queries[1].OfflineEligible || m[0].Queries[1].OfflineFactKey != "name" || m[0].Queries[1].ScopePolicy != contract.ScopeEntityOwned || len(m[0].Provides) != 2 {
				t.Fatalf("unexpected %+v", m[0])
			}
		}},
		{"bad owner", "1", []query.Definition{badOwner}, "pack.module_owner_mismatch", nil},
		{"bad name", "1", []query.Definition{badName}, "pack.query_invalid", nil},
		{"bad version", "v1", []query.Definition{plain}, "pack.version_invalid", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mods, err := GeneratePackModules(contract.PackManifest{Name: "shop", Version: tt.version}, packQueries(t, tt.defs...))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(mods) != 2 || mods[0].ID != "shop.core" || mods[1].ID != "shop.app" {
				t.Fatalf("modules %+v", mods)
			}
			if err := contract.ValidateModuleSet(mods, func(contract.SchemaRef) bool { return true }); err != nil {
				t.Fatal(err)
			}
			tt.check(t, mods)
		})
	}
}

func TestQuerySchemaDeclaresRequiredAndDefaults(t *testing.T) {
	filters := []query.Filter{
		{Field: "creation", Op: "gte", Value: "$input.since"},
		{Field: "till", Op: "eq", Value: "$input.till", Optional: true},
		{Field: "shift", Op: "eq", Value: "$input.shift"},
	}
	tests := []struct {
		name         string
		input        map[string]query.InputSpec
		wantRequired []string
		wantDefault  map[string]any
	}{
		{"required from non-optional filters", map[string]query.InputSpec{"since": {Type: "string"}, "till": {Type: "string"}, "shift": {Type: "string"}}, []string{"shift", "since"}, nil},
		{"default removes requirement and is published", map[string]query.InputSpec{"since": {Type: "string", Default: "$today"}, "till": {Type: "string"}, "shift": {Type: "string"}}, []string{"shift"}, map[string]any{"since": "$today"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var schema struct {
				Required   []string                  `json:"required"`
				Properties map[string]map[string]any `json:"properties"`
			}
			if err := json.Unmarshal(querySchema(query.Definition{Input: tc.input, Filters: filters}), &schema); err != nil {
				t.Fatal(err)
			}
			if strings.Join(schema.Required, ",") != strings.Join(tc.wantRequired, ",") {
				t.Fatalf("required = %v, want %v", schema.Required, tc.wantRequired)
			}
			for name, want := range tc.wantDefault {
				if got := schema.Properties[name]["default"]; got != want {
					t.Fatalf("default of %s = %v, want %v", name, got, want)
				}
			}
		})
	}
}
