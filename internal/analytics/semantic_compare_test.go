package analytics

import (
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	kdb "github.com/asenawritescode/kora/internal/db"
)

func mustDay(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.DateOnly, value)
	if err != nil {
		t.Fatal("parse date:", err)
	}
	return parsed
}

func TestPreviousWindowIsEqualLengthAndEndsDayBeforeRange(t *testing.T) {
	tests := []struct {
		name, from, to, wantFrom, wantTo string
	}{
		{"day range", "2026-03-10", "2026-03-10", "2026-03-09", "2026-03-09"},
		{"week range", "2026-03-02", "2026-03-08", "2026-02-23", "2026-03-01"},
		{"month range", "2026-03-01", "2026-03-31", "2026-01-29", "2026-02-28"},
		{"leap month", "2028-03-01", "2028-03-31", "2028-01-30", "2028-02-29"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotFrom, gotTo := previousWindow(mustDay(t, tc.from), mustDay(t, tc.to))
			if gotFrom.Format(time.DateOnly) != tc.wantFrom || gotTo.Format(time.DateOnly) != tc.wantTo {
				t.Fatalf("previous window = %s..%s, want %s..%s", gotFrom.Format(time.DateOnly), gotTo.Format(time.DateOnly), tc.wantFrom, tc.wantTo)
			}
		})
	}
}

func TestBucketLabelsCoverEveryBucketInRange(t *testing.T) {
	tests := []struct {
		name, from, to, granularity string
		want                        []string
	}{
		{"empty when from is after to", "2026-03-05", "2026-03-01", "day", nil},
		{"day includes empty days", "2026-03-01", "2026-03-03", "day", []string{"2026-03-01", "2026-03-02", "2026-03-03"}},
		{"week uses monday and keeps partial week", "2026-03-04", "2026-03-11", "week", []string{"2026-03-02", "2026-03-09"}},
		{"month uses first of month", "2026-01-15", "2026-03-02", "month", []string{"2026-01-01", "2026-02-01", "2026-03-01"}},
		{"quarter label", "2026-02-01", "2026-05-01", "quarter", []string{"2026-Q1", "2026-Q2"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := bucketLabels(mustDay(t, tc.from), mustDay(t, tc.to), tc.granularity)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("labels = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFillSemanticBucketsZeroFillsMissingBuckets(t *testing.T) {
	rows := []map[string]any{{"creation": "2026-03-02", "count": float64(4)}, {"creation": "2026-03-04", "count": "7"}}
	got := fillSemanticBuckets("creation", []string{"count"}, []string{"2026-03-02", "2026-03-03", "2026-03-04"}, rows)
	want := []map[string]any{
		{"creation": "2026-03-02", "count": 4.0},
		{"creation": "2026-03-03", "count": 0.0},
		{"creation": "2026-03-04", "count": 7.0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filled rows = %#v, want %#v", got, want)
	}
}

func TestSemanticDeltaNullsPercentWhenPreviousIsZero(t *testing.T) {
	twelve, six, zero := 12.0, 6.0, 0.0
	tests := []struct {
		name              string
		current, previous *float64
		wantAbs, wantPct  *float64
	}{
		{"growth", &twelve, &six, fp(6), fp(100)},
		{"decline below zero previous keeps sign", &twelve, fp(-6), fp(18), fp(300)},
		{"zero previous has no percent", &twelve, &zero, fp(12), nil},
		{"unknown total has no delta", nil, &six, nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := semanticDelta(tc.current, tc.previous)
			if !reflect.DeepEqual(got.Absolute, tc.wantAbs) || !reflect.DeepEqual(got.Percent, tc.wantPct) {
				t.Fatalf("delta = %v / %v, want %v / %v", deref(got.Absolute), deref(got.Percent), deref(tc.wantAbs), deref(tc.wantPct))
			}
		})
	}
}

func TestFoldMeasureAvgIsExactOnlyForSingleRow(t *testing.T) {
	tests := []struct {
		name        string
		aggregation string
		rows        []map[string]any
		want        *float64
	}{
		{"sum folds buckets", "sum", []map[string]any{{"v": 1.5}, {"v": "2.5"}}, fp(4)},
		{"min folds buckets", "min", []map[string]any{{"v": 3.0}, {"v": 1.0}, {"v": 2.0}}, fp(1)},
		{"max folds buckets", "max", []map[string]any{{"v": 3.0}, {"v": 1.0}}, fp(3)},
		{"avg single row", "avg", []map[string]any{{"v": 9.0}}, fp(9)},
		{"avg many rows is unknown", "avg", []map[string]any{{"v": 9.0}, {"v": 1.0}}, nil},
		{"empty window is zero", "count", nil, fp(0)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := foldMeasure(tc.aggregation, "v", tc.rows)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("fold = %v, want %v", deref(got), deref(tc.want))
			}
		})
	}
}

func TestSemanticRequestValidatesCompareMode(t *testing.T) {
	catalog := &SemanticCatalog{Models: []SemanticModel{taskComparisonModel()}}
	tests := []struct {
		compare string
		wantErr bool
	}{
		{"", false},
		{CompareModePreviousPeriod, false},
		{"year_over_year", true},
		{"PREVIOUS_PERIOD", true},
	}
	for _, tc := range tests {
		request := SemanticRequest{Queries: []ModelQuery{{Model: "task", Measures: []string{"count"}}}, Time: QueryTime{From: "2026-06-01", To: "2026-06-30"}, Compare: tc.compare}
		err := request.Validate(catalog)
		if (err != nil) != tc.wantErr {
			t.Fatalf("compare %q: err = %v, wantErr %v", tc.compare, err, tc.wantErr)
		}
	}
}

func taskComparisonModel() SemanticModel {
	return SemanticModel{
		Name: "task", SourceDoctype: "Task", TimeDimension: "creation",
		Dimensions: []SemanticDimension{{Name: "creation", Field: "creation", Type: "time"}},
		Measures:   []SemanticMeasure{{Name: "count", Aggregation: "count"}},
	}
}

// The window Jun 1 to Jul 31 is 61 days, so its previous window Apr 1 to May 31
// is month-aligned and must be read from the monthly rollup table.
func TestCompareMonthAlignedPreviousWindowUsesMonthlyTableAndNullPercent(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock:", err)
	}
	defer database.Close()
	model := taskComparisonModel()
	measureQuery := regexp.QuoteMeta("SELECT dimension, SUM(value) AS value FROM _kora_analytics_monthly WHERE site = ? AND scope_kind = ? AND legal_entity_id = ? AND doctype = ? AND metric = ? AND month >= ? AND month <= ? GROUP BY dimension ORDER BY value DESC LIMIT ?")
	mock.ExpectQuery(measureQuery).
		WithArgs("tenant-a", "tenant", "", "Task", "task_count", "2026-06-01", "2026-07-31", 200).
		WillReturnRows(sqlmock.NewRows([]string{"dimension", "value"}).AddRow("", 12.0))
	mock.ExpectQuery(measureQuery).
		WithArgs("tenant-a", "tenant", "", "Task", "task_count", "2026-04-01", "2026-05-31", 200).
		WillReturnRows(sqlmock.NewRows([]string{"dimension", "value"}))
	engine := &QueryEngine{DB: database, SiteName: "tenant-a", Dialect: kdb.Resolve("mysql")}
	response, err := engine.ResolveSemanticQuery(&SemanticCatalog{Models: []SemanticModel{model}}, SemanticRequest{
		Queries: []ModelQuery{{Model: "task", Measures: []string{"count"}}},
		Time:    QueryTime{From: "2026-06-01", To: "2026-07-31"},
		Compare: CompareModePreviousPeriod,
	})
	if err != nil {
		t.Fatal("resolve compared query:", err)
	}
	comparison := response.Queries[0].Comparison
	if comparison == nil || comparison.PreviousRange != (SemanticDateRange{From: "2026-04-01", To: "2026-05-31"}) {
		t.Fatalf("comparison = %#v", comparison)
	}
	if *comparison.Totals.Current["count"] != 12 || *comparison.Totals.Previous["count"] != 0 {
		t.Fatalf("totals = %#v", comparison.Totals)
	}
	delta := comparison.Totals.Delta["count"]
	if delta.Absolute == nil || *delta.Absolute != 12 || delta.Percent != nil {
		t.Fatalf("delta with zero previous = %#v, want absolute 12 and null percent", delta)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func fp(value float64) *float64 { return &value }

func deref[T any](value *T) any {
	if value == nil {
		return nil
	}
	return *value
}
