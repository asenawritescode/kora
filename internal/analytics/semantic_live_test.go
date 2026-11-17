package analytics

import (
	"reflect"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

func TestLiveStatementAppliesMeasureFiltersInEveryDialect(t *testing.T) {
	for _, dialectName := range []string{"mysql", "postgres"} {
		t.Run(dialectName, func(t *testing.T) {
			dialect := kdb.Resolve(dialectName)
			model := taskModel(SemanticFilter{Field: "status", Operator: "=", Values: []string{"Paid"}})
			call := semanticCall{model: &model, query: ModelQuery{Model: "task", Measures: []string{"paid_amount"}, Dimensions: []string{"customer"}, Order: OrderMeasureDesc}, time: QueryTime{From: "2026-01-01", To: "2026-01-31"}, limit: 5, registry: taskRegistry()}
			source := call.registry.Get("Task")
			query, err := liveSQL(dialect, call, model.Measures[1], "", source)
			if err != nil {
				t.Fatal("liveSQL:", err)
			}
			statement := query.statement
			for _, want := range []string{"SUM(" + dialect.QuoteIdent("amount") + ") AS " + dialect.QuoteIdent("value"), dialect.QuoteIdent("status") + " = ?", "GROUP BY " + dialect.QuoteIdent("customer"), "ORDER BY " + dialect.QuoteIdent("value") + " DESC, " + dialect.QuoteIdent("dimension") + " LIMIT ?"} {
				if !strings.Contains(statement, want) {
					t.Fatalf("statement %q missing %q", statement, want)
				}
			}
			if query.kind != "dimension" || query.dimension != "customer" {
				t.Fatalf("kind/dimension = %q/%q", query.kind, query.dimension)
			}
			if want := []any{"2026-01-01", "2026-02-01", "Paid", 5}; !reflect.DeepEqual(query.args, want) {
				t.Fatalf("args = %v, want %v", query.args, want)
			}
		})
	}
}

func TestLiveStatementRejectsUnsafeMeasureFilters(t *testing.T) {
	cases := []struct {
		name   string
		filter SemanticFilter
	}{
		{name: "unknown field", filter: SemanticFilter{Field: "missing", Operator: "=", Values: []string{"x"}}},
		{name: "unsupported operator", filter: SemanticFilter{Field: "status", Operator: "; drop", Values: []string{"x"}}},
		{name: "no values", filter: SemanticFilter{Field: "status", Operator: "in"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := taskModel(tc.filter)
			call := semanticCall{model: &model, query: ModelQuery{Model: "task", Measures: []string{"paid_amount"}}, time: QueryTime{From: "2026-01-01", To: "2026-01-31"}, registry: taskRegistry()}
			if _, err := liveSQL(kdb.Resolve("mysql"), call, model.Measures[1], "", call.registry.Get("Task")); err == nil {
				t.Fatal("expected unsafe measure filter to be rejected")
			}
		})
	}
}

// entityTaskRegistry is taskRegistry with Task owned by legal entity.
func entityTaskRegistry() *doctype.Registry {
	registry := taskRegistry()
	task := registry.Get("Task")
	task.BusinessScopePolicy = contract.ScopeEntityOwned
	task.BusinessScopeField = "legal_entity"
	return registry
}

func TestLiveScopeClauseMirrorsRollupKeys(t *testing.T) {
	entityOwned := &doctype.DocType{Name: "Task", BusinessScopePolicy: contract.ScopeEntityOwned, BusinessScopeField: "legal_entity"}
	tenantOwned := &doctype.DocType{Name: "Task"}
	entityLE1 := contract.BusinessScope{Kind: contract.ScopeKindLegalEntity, LegalEntityID: "LE-1"}
	tenant := contract.BusinessScope{Kind: contract.ScopeKindTenant}
	cases := []struct {
		name      string
		source    *doctype.DocType
		scope     contract.BusinessScope
		wantSQL   func(kdb.Dialect) string
		wantArgs  []any
		wantError bool
	}{
		{name: "entity caller reads its own legal entity", source: entityOwned, scope: entityLE1, wantSQL: func(d kdb.Dialect) string { return d.QuoteIdent("legal_entity") + " = ?" }, wantArgs: []any{"LE-1"}},
		{name: "tenant caller on entity source has no rows", source: entityOwned, scope: tenant, wantSQL: func(kdb.Dialect) string { return "1 = 0" }},
		{name: "entity caller without id has no rows", source: entityOwned, scope: contract.BusinessScope{Kind: contract.ScopeKindLegalEntity}, wantSQL: func(kdb.Dialect) string { return "1 = 0" }},
		{name: "entity source without scope field fails", source: &doctype.DocType{Name: "Task", BusinessScopePolicy: contract.ScopeEntityOwned}, scope: entityLE1, wantError: true},
		{name: "tenant caller on tenant source is unrestricted", source: tenantOwned, scope: tenant, wantSQL: func(kdb.Dialect) string { return "" }},
		{name: "zero scope is tenant", source: tenantOwned, scope: contract.BusinessScope{}, wantSQL: func(kdb.Dialect) string { return "" }},
		{name: "entity caller on tenant source has no rows", source: tenantOwned, scope: entityLE1, wantSQL: func(kdb.Dialect) string { return "1 = 0" }},
	}
	for _, dialectName := range []string{"mysql", "postgres"} {
		dialect := kdb.Resolve(dialectName)
		for _, tc := range cases {
			t.Run(dialectName+" "+tc.name, func(t *testing.T) {
				sql, args, err := liveScopeClause(dialect, tc.scope, tc.source)
				if (err != nil) != tc.wantError {
					t.Fatalf("liveScopeClause() error = %v, wantError %v", err, tc.wantError)
				}
				if tc.wantError {
					return
				}
				if want := tc.wantSQL(dialect); sql != want {
					t.Fatalf("clause = %q, want %q", sql, want)
				}
				if !reflect.DeepEqual(args, tc.wantArgs) {
					t.Fatalf("args = %v, want %v", args, tc.wantArgs)
				}
			})
		}
	}
}

func TestLiveStatementScopesToLegalEntityInEveryDialect(t *testing.T) {
	for _, dialectName := range []string{"mysql", "postgres"} {
		t.Run(dialectName, func(t *testing.T) {
			dialect := kdb.Resolve(dialectName)
			model := taskModel(SemanticFilter{Field: "status", Operator: "=", Values: []string{"Paid"}})
			call := semanticCall{model: &model, query: ModelQuery{Model: "task", Measures: []string{"paid_amount"}, Dimensions: []string{"customer"}}, time: QueryTime{From: "2026-01-01", To: "2026-01-31"}, limit: 5, registry: entityTaskRegistry(), scope: contract.BusinessScope{Kind: contract.ScopeKindLegalEntity, LegalEntityID: "LE-1"}}
			query, err := liveSQL(dialect, call, model.Measures[1], "", call.registry.Get("Task"))
			if err != nil {
				t.Fatal("liveSQL:", err)
			}
			if want := dialect.QuoteIdent("legal_entity") + " = ?"; !strings.Contains(query.statement, want) {
				t.Fatalf("statement %q missing %q", query.statement, want)
			}
			if want := []any{"2026-01-01", "2026-02-01", "LE-1", "Paid", 5}; !reflect.DeepEqual(query.args, want) {
				t.Fatalf("args = %v, want %v", query.args, want)
			}
		})
	}
}
