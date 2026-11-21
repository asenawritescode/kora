package api

import (
	"reflect"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/metrics"
)

// questionCatalog offers one record type with a fact model (Expense) and one metric over it.
func questionCatalog() metrics.Catalog {
	expense := &doctype.DocType{Name: "Expense", Facts: &doctype.FactModel{Time: "creation",
		Measures:   []doctype.FactMeasure{{Field: "amount"}},
		Dimensions: []doctype.FactDimension{{Field: "category"}}}}
	defs := []metrics.Metric{{Name: "spend", Label: "Spend", Type: metrics.KindSimple, Model: "Expense",
		Measure: &metrics.Measure{Agg: "sum", Field: "amount"}}}
	return metrics.NewCatalog(defs, []*doctype.DocType{expense})
}

func TestCheckSavedQuestion(t *testing.T) {
	cat := questionCatalog()
	tests := []struct {
		name    string
		q       contract.SavedQuestion
		models  []string
		wantErr bool
	}{
		{name: "count of a record type", q: contract.SavedQuestion{RecordType: "Expense"}, models: []string{"Expense"}},
		{name: "sum measure by a dimension", q: contract.SavedQuestion{RecordType: "Expense", Measure: "sum:amount", By: []string{"category"}, Grain: "month"}, models: []string{"Expense"}},
		{name: "period, compare and top", q: contract.SavedQuestion{RecordType: "Expense", From: "2026-11-01", To: "2026-11-30", Compare: "previous_period", Top: 5}, models: []string{"Expense"}},
		{name: "filter on a fact field", q: contract.SavedQuestion{RecordType: "Expense", Measure: "sum:amount", Filters: []contract.MetricFilter{{Field: "category", Op: "=", Value: []byte(`"fuel"`)}}}, models: []string{"Expense"}},
		{name: "metric by a dimension", q: contract.SavedQuestion{Metric: "spend", By: []string{"category"}}, models: []string{"Expense"}},
		{name: "unknown record type", q: contract.SavedQuestion{RecordType: "Ghost"}, wantErr: true},
		{name: "measure field removed", q: contract.SavedQuestion{RecordType: "Expense", Measure: "sum:cost"}, wantErr: true},
		{name: "by field removed", q: contract.SavedQuestion{RecordType: "Expense", By: []string{"supplier"}}, wantErr: true},
		{name: "unknown metric", q: contract.SavedQuestion{Metric: "ghost"}, wantErr: true},
		{name: "metric with a measure", q: contract.SavedQuestion{Metric: "spend", Measure: "count"}, wantErr: true},
		{name: "record type and metric", q: contract.SavedQuestion{RecordType: "Expense", Metric: "spend"}, wantErr: true},
		{name: "neither record type nor metric", q: contract.SavedQuestion{}, wantErr: true},
		{name: "bad grain", q: contract.SavedQuestion{RecordType: "Expense", Grain: "hour"}, wantErr: true},
		{name: "bad compare", q: contract.SavedQuestion{RecordType: "Expense", Compare: "week_before"}, wantErr: true},
		{name: "negative top", q: contract.SavedQuestion{RecordType: "Expense", Top: -1}, wantErr: true},
		{name: "bad period date", q: contract.SavedQuestion{RecordType: "Expense", From: "2026-13-01", To: "2026-11-30"}, wantErr: true},
		{name: "filter on an unknown field", q: contract.SavedQuestion{RecordType: "Expense", Filters: []contract.MetricFilter{{Field: "ghost", Op: "=", Value: []byte(`1`)}}}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			models, err := checkSavedQuestion(cat, tc.q)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if !tc.wantErr && !reflect.DeepEqual(models, tc.models) {
				t.Fatalf("models = %v, want %v", models, tc.models)
			}
		})
	}
}

func TestReportNameKey(t *testing.T) {
	tests := map[string]string{
		"Expenses by Category": "expenses by category",
		"  Stock moves  ":      "stock moves",
		"ÉCHEC":                "échec",
	}
	for in, want := range tests {
		if got := reportNameKey(in); got != want {
			t.Errorf("reportNameKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRecheckKeepsBrokenSince checks that a report stays broken from the first moment it broke, that a
// different reason is a new break, and that a question that checks again clears the state.
func TestRecheckKeepsBrokenSince(t *testing.T) {
	cat := questionCatalog()
	first := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	later := first.Add(time.Hour)
	rep := savedReport{ID: "r1", Question: contract.SavedQuestion{RecordType: "Expense", Measure: "sum:cost"}}

	rep = rep.recheck(cat, first)
	if rep.BrokenReason == "" || !rep.BrokenAt.Equal(first) {
		t.Fatalf("first check: reason %q at %v, want broken at %v", rep.BrokenReason, rep.BrokenAt, first)
	}
	reason := rep.BrokenReason
	if rep = rep.recheck(cat, later); !rep.BrokenAt.Equal(first) || rep.BrokenReason != reason {
		t.Fatalf("same break moved: at %v, reason %q", rep.BrokenAt, rep.BrokenReason)
	}
	rep.Question = contract.SavedQuestion{RecordType: "Expense", Measure: "sum:amount"}
	rep = rep.recheck(cat, later)
	if rep.BrokenReason != "" || !rep.BrokenAt.IsZero() || !reflect.DeepEqual(rep.RecordTypes, []string{"Expense"}) {
		t.Fatalf("fixed report: reason %q, at %v, types %v", rep.BrokenReason, rep.BrokenAt, rep.RecordTypes)
	}
}
