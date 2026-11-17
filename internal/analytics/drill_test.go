package analytics

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func drillCatalog(paidOnly bool) *SemanticCatalog {
	var filters []SemanticFilter
	if paidOnly {
		filters = []SemanticFilter{{Field: "status", Operator: "=", Values: []string{"Paid"}}}
	}
	model := taskModel(filters...)
	model.Measures = append(model.Measures, SemanticMeasure{Name: "paid_count", Aggregation: "count", Filters: filters})
	return &SemanticCatalog{Models: []SemanticModel{model}}
}

func TestBuildDrillMapsNumbersToListFilters(t *testing.T) {
	jan := QueryTime{From: "2026-01-01", To: "2026-01-31", Granularity: "day"}
	cases := []struct {
		name    string
		request DrillRequest
		scope   contract.BusinessScope
		entity  bool
		want    string
		wantErr bool
	}{
		{name: "total count window", request: DrillRequest{Model: "task", Time: jan},
			want: `[["transaction_date",">=","2026-01-01"],["transaction_date","<","2026-02-01"]]`},
		{name: "category value with measure filter", request: DrillRequest{Model: "task", Measure: "paid_count", Dimension: "customer", Value: "CUST-1", Time: jan},
			want: `[["status","=","Paid"],["customer","=","CUST-1"],["transaction_date",">=","2026-01-01"],["transaction_date","<","2026-02-01"]]`},
		{name: "empty category is null", request: DrillRequest{Model: "task", Dimension: "customer", Time: jan},
			want: `[["customer","is",null],["transaction_date",">=","2026-01-01"],["transaction_date","<","2026-02-01"]]`},
		{name: "day bucket inside range", request: DrillRequest{Model: "task", Dimension: "transaction_date", Value: "2026-01-05", Time: jan},
			want: `[["transaction_date",">=","2026-01-05"],["transaction_date","<","2026-01-06"]]`},
		{name: "month bucket clipped to range", request: DrillRequest{Model: "task", Dimension: "transaction_date", Value: "2026-01-01", Time: QueryTime{From: "2026-01-10", To: "2026-02-03", Granularity: "month"}},
			want: `[["transaction_date",">=","2026-01-10"],["transaction_date","<","2026-02-01"]]`},
		{name: "week bucket starts on monday", request: DrillRequest{Model: "task", Dimension: "transaction_date", Value: "2026-01-05", Time: QueryTime{From: "2026-01-01", To: "2026-01-31", Granularity: "week"}},
			want: `[["transaction_date",">=","2026-01-05"],["transaction_date","<","2026-01-12"]]`},
		{name: "quarter bucket", request: DrillRequest{Model: "task", Dimension: "transaction_date", Value: "2026-Q1", Time: QueryTime{From: "2026-01-01", To: "2026-12-31", Granularity: "quarter"}},
			want: `[["transaction_date",">=","2026-01-01"],["transaction_date","<","2026-04-01"]]`},
		{name: "bucket outside range", request: DrillRequest{Model: "task", Dimension: "transaction_date", Value: "2026-03-01", Time: jan}, wantErr: true},
		{name: "bucket label not on granularity", request: DrillRequest{Model: "task", Dimension: "transaction_date", Value: "2026-01-07", Time: QueryTime{From: "2026-01-01", To: "2026-01-31", Granularity: "week"}}, wantErr: true},
		{name: "like cannot be listed exactly", request: DrillRequest{Model: "task", Dimension: "customer", Value: "x", Time: jan, Filters: []SemanticFilter{{Field: "customer", Operator: "like", Values: []string{"CUST"}}}}, wantErr: true},
		{name: "value without dimension", request: DrillRequest{Model: "task", Value: "x", Time: jan}, wantErr: true},
		{name: "unknown model", request: DrillRequest{Model: "nope", Time: jan}, wantErr: true},
		{name: "unknown measure", request: DrillRequest{Model: "task", Measure: "nope", Time: jan}, wantErr: true},
		{name: "legal entity scope adds entity filter", request: DrillRequest{Model: "task", Time: jan}, scope: entityLE1(), entity: true,
			want: `[["transaction_date",">=","2026-01-01"],["transaction_date","<","2026-02-01"],["legal_entity","=","LE-1"]]`},
		{name: "tenant scope on entity source has no records", request: DrillRequest{Model: "task", Time: jan}, scope: contract.BusinessScope{Kind: contract.ScopeKindTenant}, entity: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry := taskRegistry()
			if tc.entity {
				registry = entityTaskRegistry()
			}
			catalog := drillCatalog(tc.request.Measure == "paid_count")
			scope := tc.scope
			if scope.Kind == "" {
				scope = contract.BusinessScope{Kind: contract.ScopeKindTenant}
			}
			drill, err := BuildDrill(catalog, registry, tc.request, scope)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("BuildDrill() = %v, want error", drill)
				}
				return
			}
			if err != nil {
				t.Fatal("BuildDrill():", err)
			}
			if got := fmt.Sprint(drill.Filters); got != fmt.Sprint(decodeWant(t, tc.want)) {
				t.Fatalf("filters = %v, want %s", got, tc.want)
			}
			if drill.Doctype != "Task" {
				t.Fatalf("doctype = %q, want Task", drill.Doctype)
			}
		})
	}
}

// decodeWant turns the expected JSON into the same Go shape BuildDrill returns.
func decodeWant(t *testing.T, raw string) [][]any {
	t.Helper()
	var out [][]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal("decode expected filters:", err)
	}
	return out
}

func entityLE1() contract.BusinessScope {
	return contract.BusinessScope{Kind: contract.ScopeKindLegalEntity, LegalEntityID: "LE-1"}
}

func TestReportClientFiltersMergeByQueryID(t *testing.T) {
	report := ReportDefinition{Name: "sales", Label: "Sales", Queries: []ReportQuery{
		{ID: "a", Model: "task", Measures: []string{"count"}, Filters: []SemanticFilter{{Field: "customer", Operator: "=", Values: []string{"CUST-2"}}}},
		{ID: "b", Model: "task", Measures: []string{"count"}},
	}}
	catalog := &SemanticCatalog{Models: []SemanticModel{taskModel()}}
	cases := []struct {
		name    string
		client  []ModelQuery
		wantErr bool
		wantLen int
	}{
		{name: "no client filters", client: nil, wantLen: 1},
		{name: "matched by id", client: []ModelQuery{{ID: "a", Filters: []SemanticFilter{{Field: "customer", Operator: "=", Values: []string{"CUST-1"}}}}}, wantLen: 2},
		{name: "unknown id", client: []ModelQuery{{ID: "zz"}}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := report.ValidateQuery(catalog, tc.client)
			if tc.wantErr {
				if err == nil {
					t.Fatal("ValidateQuery() = nil, want unknown query id error")
				}
				return
			}
			if err != nil {
				t.Fatal("ValidateQuery():", err)
			}
			request := SemanticRequest{Queries: tc.client}
			report.ApplyTo(&request)
			if got := len(request.Queries[0].Filters); got != tc.wantLen {
				t.Fatalf("query a filters = %d, want %d", got, tc.wantLen)
			}
			if got := len(report.Queries[0].Filters); got != 1 {
				t.Fatalf("report definition mutated: %d filters", got)
			}
		})
	}
}
