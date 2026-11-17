//go:build integration

package analytics

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	_ "github.com/lib/pq"
)

// compareSeed is one rollup row. Daily rows use the date column, monthly rows
// the month column.
type compareSeed struct {
	monthly bool
	date    string
	value   float64
}

type compareCase struct {
	name, from, to, granularity string
	current, previous           []compareSeed
	wantBuckets                 []string
	wantCurrent, wantPrevious   []float64
	wantCurrentTotal            float64
	wantPreviousTotal           float64
	wantPercent                 *float64
}

// Expected values per case come from the spec: zero-filled buckets for both
// windows, previous window of equal length, percent null when previous is 0.
func compareCases() []compareCase {
	return []compareCase{
		{
			name: "day", from: "2026-03-02", to: "2026-03-08", granularity: "day",
			current:     []compareSeed{{date: "2026-03-02", value: 4}, {date: "2026-03-06", value: 3}},
			previous:    []compareSeed{{date: "2026-02-24", value: 1}, {date: "2026-02-25", value: 2}},
			wantBuckets: []string{"2026-03-02", "2026-03-03", "2026-03-04", "2026-03-05", "2026-03-06", "2026-03-07", "2026-03-08"},
			wantCurrent: []float64{4, 0, 0, 0, 3, 0, 0}, wantPrevious: []float64{0, 1, 2, 0, 0, 0, 0},
			wantCurrentTotal: 7, wantPreviousTotal: 3, wantPercent: fp(400.0 / 3),
		},
		{
			name: "week", from: "2026-03-02", to: "2026-03-22", granularity: "week",
			current:     []compareSeed{{date: "2026-03-03", value: 2}, {date: "2026-03-10", value: 5}},
			previous:    []compareSeed{{date: "2026-02-10", value: 1}},
			wantBuckets: []string{"2026-03-02", "2026-03-09", "2026-03-16"},
			wantCurrent: []float64{2, 5, 0}, wantPrevious: []float64{1, 0, 0},
			wantCurrentTotal: 7, wantPreviousTotal: 1, wantPercent: fp(600),
		},
		{
			name: "month uses monthly rollup for aligned current window", from: "2026-02-01", to: "2026-04-30", granularity: "month",
			current:     []compareSeed{{monthly: true, date: "2026-02-01", value: 10}, {monthly: true, date: "2026-04-01", value: 4}},
			previous:    []compareSeed{{date: "2025-12-15", value: 6}},
			wantBuckets: []string{"2026-02-01", "2026-03-01", "2026-04-01"},
			wantCurrent: []float64{10, 0, 4}, wantPrevious: []float64{0, 6, 0},
			wantCurrentTotal: 14, wantPreviousTotal: 6, wantPercent: fp(400.0 / 3),
		},
		{
			name: "empty range with zero previous has null percent", from: "2026-03-02", to: "2026-03-04", granularity: "day",
			wantBuckets: []string{"2026-03-02", "2026-03-03", "2026-03-04"},
			wantCurrent: []float64{0, 0, 0}, wantPrevious: []float64{0, 0, 0},
			wantCurrentTotal: 0, wantPreviousTotal: 0, wantPercent: nil,
		},
	}
}

// TestCompareIntegrationMySQL and TestCompareIntegrationPostgres run the same
// cases against real rollup tables. Each case uses its own site so reruns on a
// shared database stay isolated.
func TestCompareIntegrationMySQL(t *testing.T) {
	runCompareIntegration(t, "KORA_ANALYTICS_COMPARE_MYSQL_DSN", "mysql", "mysql")
}

func TestCompareIntegrationPostgres(t *testing.T) {
	runCompareIntegration(t, "KORA_ANALYTICS_COMPARE_POSTGRES_DSN", "postgres", "postgres")
}

func runCompareIntegration(t *testing.T, dsnVar, driver, dialectName string) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(dsnVar))
	if dsn == "" {
		t.Fatalf("set %s to a disposable %s database", dsnVar, dialectName)
	}
	database, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal("open database:", err)
	}
	defer database.Close()
	dialect := db.Resolve(dialectName)
	if err := BootstrapTables(context.Background(), database, dialect); err != nil {
		t.Fatal("bootstrap analytics tables:", err)
	}
	for _, tc := range compareCases() {
		t.Run(tc.name, func(t *testing.T) {
			site := fmt.Sprintf("compare-%d", time.Now().UnixNano())
			for _, seed := range append(append([]compareSeed{}, tc.current...), tc.previous...) {
				insertCompareSeed(t, database, dialect, site, seed)
			}
			engine := &QueryEngine{DB: database, SiteName: site, Dialect: dialect}
			response, err := engine.ResolveSemanticQuery(&SemanticCatalog{Models: []SemanticModel{taskComparisonModel()}}, SemanticRequest{
				Queries: []ModelQuery{{Model: "task", Measures: []string{"count"}}},
				Time:    QueryTime{From: tc.from, To: tc.to, Granularity: tc.granularity},
				Compare: CompareModePreviousPeriod,
			})
			if err != nil {
				t.Fatal("resolve compared query:", err)
			}
			result := response.Queries[0]
			if got := bucketColumn(result.Rows); !reflect.DeepEqual(got, tc.wantBuckets) {
				t.Fatalf("buckets = %v, want %v", got, tc.wantBuckets)
			}
			if got := valueColumn(result.Rows, "count"); !reflect.DeepEqual(got, tc.wantCurrent) {
				t.Fatalf("current series = %v, want %v", got, tc.wantCurrent)
			}
			comparison := result.Comparison
			if got := valueColumn(comparison.PreviousRows, "count"); !reflect.DeepEqual(got, tc.wantPrevious) {
				t.Fatalf("previous series = %v, want %v", got, tc.wantPrevious)
			}
			if *comparison.Totals.Current["count"] != tc.wantCurrentTotal || *comparison.Totals.Previous["count"] != tc.wantPreviousTotal {
				t.Fatalf("totals current/previous = %v/%v, want %v/%v", *comparison.Totals.Current["count"], *comparison.Totals.Previous["count"], tc.wantCurrentTotal, tc.wantPreviousTotal)
			}
			percent := comparison.Totals.Delta["count"].Percent
			if (percent == nil) != (tc.wantPercent == nil) || (percent != nil && absDiff(*percent, *tc.wantPercent) > 1e-6) {
				t.Fatalf("percent = %v, want %v", deref(percent), deref(tc.wantPercent))
			}
		})
	}
}

func insertCompareSeed(t *testing.T, database *sql.DB, dialect db.Dialect, site string, seed compareSeed) {
	t.Helper()
	table, column := "_kora_analytics_daily", "date"
	if seed.monthly {
		table, column = "_kora_analytics_monthly", "month"
	}
	query := fmt.Sprintf("INSERT INTO %s (site, scope_kind, legal_entity_id, doctype, metric, dimension, %s, value) VALUES (?, 'tenant', '', 'Task', 'task_count', '', ?, ?)", table, column)
	if _, err := database.Exec(db.Rebind(dialect, query), site, seed.date, seed.value); err != nil {
		t.Fatal("insert rollup seed:", err)
	}
}

func bucketColumn(rows []map[string]any) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, fmt.Sprint(row["creation"]))
	}
	return out
}

func valueColumn(rows []map[string]any, name string) []float64 {
	out := make([]float64, 0, len(rows))
	for _, row := range rows {
		value, _ := semanticNumber(row[name])
		out = append(out, value)
	}
	return out
}

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}
