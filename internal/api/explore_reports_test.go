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
		{name: "bad compare", q: contract.SavedQuestion{RecordType: "Expense", Compare: "last_year"}, wantErr: true},
		{name: "week before compare", q: contract.SavedQuestion{RecordType: "Expense", Compare: "week_before"}, models: []string{"Expense"}},
		{name: "preset period", q: contract.SavedQuestion{RecordType: "Expense", Period: "last_week", Compare: "previous_period"}, models: []string{"Expense"}},
		{name: "unknown preset", q: contract.SavedQuestion{RecordType: "Expense", Period: "fortnight"}, wantErr: true},
		{name: "preset with dates", q: contract.SavedQuestion{RecordType: "Expense", Period: "last_week", From: "2026-11-01", To: "2026-11-30"}, wantErr: true},
		{name: "negative top", q: contract.SavedQuestion{RecordType: "Expense", Top: -1}, wantErr: true},
		{name: "bad period date", q: contract.SavedQuestion{RecordType: "Expense", From: "2026-13-01", To: "2026-11-30"}, wantErr: true},
		{name: "filter on an unknown field", q: contract.SavedQuestion{RecordType: "Expense", Filters: []contract.MetricFilter{{Field: "ghost", Op: "=", Value: []byte(`1`)}}}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			models, err := checkSavedQuestion(cat, allJoin(cat), tc.q)
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
// different missing piece is a new break, and that a question that checks again clears the state.
func TestRecheckKeepsBrokenSince(t *testing.T) {
	cat := questionCatalog()
	first := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	later := first.Add(time.Hour)
	rep := savedReport{ID: "r1", Question: contract.SavedQuestion{RecordType: "Expense", Measure: "sum:cost"}}

	rep = rep.recheck(cat, allJoin(cat), first)
	want := &contract.SavedReportBroken{Kind: brokenMissingField, RecordType: "Expense", Field: "sum:cost", Label: "sum:cost"}
	if rep.Broken == nil || *rep.Broken != *want || !rep.BrokenAt.Equal(first) {
		t.Fatalf("first check: broken %+v at %v, want %+v at %v", rep.Broken, rep.BrokenAt, want, first)
	}
	if rep = rep.recheck(cat, allJoin(cat), later); !rep.BrokenAt.Equal(first) || *rep.Broken != *want {
		t.Fatalf("same break moved: at %v, broken %+v", rep.BrokenAt, rep.Broken)
	}
	rep.Question = contract.SavedQuestion{RecordType: "Expense", Measure: "sum:amount"}
	rep = rep.recheck(cat, allJoin(cat), later)
	if rep.Broken != nil || !rep.BrokenAt.IsZero() || !reflect.DeepEqual(rep.RecordTypes, []string{"Expense"}) {
		t.Fatalf("fixed report: broken %+v, at %v, types %v", rep.Broken, rep.BrokenAt, rep.RecordTypes)
	}
}

// TestBrokenOfPiece checks the piece each lost part of a question names.
func TestBrokenOfPiece(t *testing.T) {
	cat := questionCatalog()
	tests := []struct {
		name string
		q    contract.SavedQuestion
		want contract.SavedReportBroken
	}{
		{"record type", contract.SavedQuestion{RecordType: "Ghost"},
			contract.SavedReportBroken{Kind: brokenMissingRecordType, RecordType: "Ghost", Label: "Ghost"}},
		{"measure", contract.SavedQuestion{RecordType: "Expense", Measure: "sum:cost"},
			contract.SavedReportBroken{Kind: brokenMissingField, RecordType: "Expense", Field: "sum:cost", Label: "sum:cost"}},
		{"by dimension", contract.SavedQuestion{RecordType: "Expense", By: []string{"ghost"}},
			contract.SavedReportBroken{Kind: brokenMissingField, RecordType: "Expense", Field: "ghost", Label: "ghost"}},
		{"metric", contract.SavedQuestion{Metric: "ghost"},
			contract.SavedReportBroken{Kind: brokenMissingMetric, Field: "ghost", Label: "ghost"}},
		{"settings", contract.SavedQuestion{RecordType: "Expense", Grain: "hour"},
			contract.SavedReportBroken{Kind: brokenInvalidQuestion, Label: `grain "hour" is not day, week, month or range`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := checkSavedQuestion(cat, allJoin(cat), tc.q)
			if err == nil {
				t.Fatal("question checked, want an error")
			}
			if got := brokenOf(err); *got != tc.want {
				t.Fatalf("broken = %+v, want %+v", *got, tc.want)
			}
		})
	}
}

// TestBrokenColumnRoundTrip checks that the broken column keeps the piece, and that text from before it was
// structured still loads.
func TestBrokenColumnRoundTrip(t *testing.T) {
	want := &contract.SavedReportBroken{Kind: brokenMissingField, RecordType: "Expense", Field: "amount", Label: "amount"}
	stored, err := encodeBroken(want)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeBroken(stored); *got != *want {
		t.Fatalf("round trip = %+v, want %+v", *got, *want)
	}
	if got := decodeBroken(""); got != nil {
		t.Fatalf("running report decoded as broken: %+v", got)
	}
	if got := decodeBroken("Old text reason"); got.Kind != brokenInvalidQuestion || got.Label != "Old text reason" {
		t.Fatalf("text column = %+v", got)
	}
}

// allJoin lets every record type of cat be joined, as the activation check does.
func allJoin(cat metrics.Catalog) map[string]bool {
	join := map[string]bool{}
	for name := range cat.Models {
		join[name] = true
	}
	return join
}

// TestCheckSavedQuestionHopTypes checks that a question reading through a Link lists the linked record type
// with the record types it uses, and that a join the caller may not make refuses the path.
func TestCheckSavedQuestionHopTypes(t *testing.T) {
	supplier := &doctype.DocType{Name: "Supplier", Fields: []doctype.Field{{Fieldname: "town", Fieldtype: "Data"}}}
	expense := &doctype.DocType{Name: "Expense", Facts: &doctype.FactModel{Time: "creation",
		Measures:   []doctype.FactMeasure{{Field: "amount"}},
		Dimensions: []doctype.FactDimension{{Field: "category"}, {Field: "supplier"}}},
		Fields: []doctype.Field{{Fieldname: "supplier", Fieldtype: "Link", Options: "Supplier"}, {Fieldname: "category", Fieldtype: "Select"}}}
	defs := []metrics.Metric{{Name: "spend", Label: "Spend", Type: metrics.KindSimple, Model: "Expense",
		Measure: &metrics.Measure{Agg: "sum", Field: "amount"}}}
	cat := metrics.NewCatalog(defs, []*doctype.DocType{expense, supplier})
	tests := []struct {
		name   string
		q      contract.SavedQuestion
		join   map[string]bool
		want   []string
		wantOK bool
	}{
		{name: "no link", q: contract.SavedQuestion{RecordType: "Expense", By: []string{"category"}}, join: map[string]bool{"Supplier": true}, want: []string{"Expense"}, wantOK: true},
		{name: "by a linked field lists the target", q: contract.SavedQuestion{RecordType: "Expense", By: []string{"supplier.town"}}, join: map[string]bool{"Supplier": true}, want: []string{"Expense", "Supplier"}, wantOK: true},
		{name: "metric by a linked field", q: contract.SavedQuestion{Metric: "spend", By: []string{"supplier.town"}}, join: map[string]bool{"Supplier": true}, want: []string{"Expense", "Supplier"}, wantOK: true},
		{name: "join not allowed refuses the path", q: contract.SavedQuestion{RecordType: "Expense", By: []string{"supplier.town"}}, join: map[string]bool{}, wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := checkSavedQuestion(cat, tc.join, tc.q)
			if (err == nil) != tc.wantOK {
				t.Fatalf("error = %v, want ok %v", err, tc.wantOK)
			}
			if tc.wantOK && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("record types = %v, want %v", got, tc.want)
			}
		})
	}
}
