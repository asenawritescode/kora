//go:build integration

package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/eventstream"
	"github.com/asenawritescode/kora/internal/facts"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// openLane admits every summary refresh.
type openLane struct{}

func (openLane) AcquireMaintenance(context.Context) (func(), error) { return func() {}, nil }

// sale is one POS sale as the seed writes it. Creation is UTC; the business date is Nairobi.
type sale struct {
	record, owner, created, status, total, register, entity string
}

// seedSales: SAL-4 is held, so it has no facts. SAL-5 is created 22:30 UTC on 19 November,
// which is 01:30 on 20 November in Nairobi, so its business date is 20 November. SAL-6 is a
// return on 19 November only, so that day has facts but no receipts. SAL-7 is on 17 November
// at register R3, outside the 18-20 range of most queries; it is the third group for top-N and
// the previous period of the compare query.
var seedSales = []sale{
	{"SAL-1", "u1", "2026-11-18 09:00:00", "Paid", "100.00", "R1", "E1"},
	{"SAL-2", "u2", "2026-11-18 10:00:00", "Paid", "50.00", "R2", "E2"},
	{"SAL-3", "u1", "2026-11-18 11:00:00", "Return", "30.00", "R1", "E1"},
	{"SAL-4", "u2", "2026-11-18 12:00:00", "Held", "999.00", "R1", "E1"},
	{"SAL-5", "u2", "2026-11-19 22:30:00", "Paid", "200.00", "R1", "E1"},
	{"SAL-6", "u2", "2026-11-19 10:00:00", "Return", "25.00", "R1", "E1"},
	{"SAL-7", "u3", "2026-11-17 10:00:00", "Paid", "40.00", "R3", "E1"},
}

// TestMetricsQueryAcrossDialects seeds POS sales through the real fact consumer, refreshes the
// summaries, and checks the pack's Sales, Receipts and Average sale against a hand computation.
func TestMetricsQueryAcrossDialects(t *testing.T) {
	tests := []struct {
		driver, dialect, envVar string
	}{
		{"mysql", "mysql", "KORA_METRICS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_METRICS_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range tests {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run", tc.envVar)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			dialect := db.Resolve(tc.dialect)
			engine := seed(t, database.DB, dialect)
			for _, q := range queries() {
				t.Run(q.name, func(t *testing.T) {
					res, err := engine.Query(context.Background(), q.query)
					if err != nil {
						t.Fatalf("query: %v", err)
					}
					if got := render(res.Rows); strings.Join(got, "\n") != strings.Join(q.want, "\n") {
						t.Fatalf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(q.want, "\n"))
					}
					if q.wantPrevious != nil {
						if got := render(res.Previous); strings.Join(got, "\n") != strings.Join(q.wantPrevious, "\n") {
							t.Fatalf("previous:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(q.wantPrevious, "\n"))
						}
					}
				})
			}
		})
	}
}

// seed creates the sales table, the site settings, and the fact tables through the consumer, then
// refreshes the summaries. It returns an engine over the seeded database.
func seed(t *testing.T, conn *sql.DB, dialect db.Dialect) Engine {
	t.Helper()
	ctx := context.Background()
	q := dialect.QuoteIdent
	setup := []string{
		fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, owner VARCHAR(140) NOT NULL, creation TIMESTAMP NOT NULL, status VARCHAR(40) NOT NULL, total DECIMAL(18,2) NOT NULL, register VARCHAR(40) NOT NULL)", q("tabSale")),
		"CREATE TABLE _kora_site_setting (site VARCHAR(140) NOT NULL, setting_key VARCHAR(140) NOT NULL, setting_value VARCHAR(140) NOT NULL, PRIMARY KEY (site, setting_key))",
		db.Rebind(dialect, "INSERT INTO _kora_site_setting (site, setting_key, setting_value) VALUES (?, ?, ?)"),
	}
	for _, stmt := range setup {
		var args []any
		if strings.HasPrefix(stmt, "INSERT") {
			args = []any{"test", tenanttime.KeyTimeZone, "Africa/Nairobi"}
		}
		if _, err := conn.ExecContext(ctx, stmt, args...); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	dt, err := doctype.ParseFile("../../config/pos/doctypes/sale.yaml")
	if err != nil {
		t.Fatalf("parse sale: %v", err)
	}
	// The seed is entity-owned so the entity scope has a column to read.
	dt.BusinessScopeField = "legal_entity"
	dt.Fields = append(dt.Fields, doctype.Field{Fieldname: "legal_entity", Fieldtype: "Link"})
	defs, err := ParseDirectory("../../config/pos/metrics")
	if err != nil {
		t.Fatalf("parse metrics: %v", err)
	}
	// Net sales is a test-only derived metric: the pack keeps its four metrics.
	defs = append(defs, Metric{Name: "net_sales", Label: "Net sales", Type: KindDerived, Formula: "sales - refunds"})
	// Sales to date is a test-only cumulative: paid totals from the start of the range.
	defs = append(defs, Metric{Name: "sales_to_date", Label: "Sales to date", Type: KindCumulative, Model: "Sale",
		Measure: &Measure{Agg: aggSum, Field: "total"}, Filters: []Filter{{Field: "status", Op: "=", Value: "Paid"}}})
	if err := Validate(defs, []*doctype.DocType{dt}); err != nil {
		t.Fatalf("pack does not validate: %v", err)
	}
	reg := doctype.NewRegistry()
	reg.Register(dt)
	builder := &facts.Builder{DB: conn, Dialect: dialect, Registry: reg}
	for i, s := range seedSales {
		insert := db.Rebind(dialect, fmt.Sprintf("INSERT INTO %s (name, owner, creation, status, total, register) VALUES (?, ?, ?, ?, ?, ?)", q("tabSale")))
		if _, err := conn.ExecContext(ctx, insert, s.record, s.owner, s.created, s.status, s.total, s.register); err != nil {
			t.Fatalf("insert %s: %v", s.record, err)
		}
		consume(ctx, t, conn, builder, int64(i+1), s)
	}
	refresher := &facts.Refresher{DB: conn, Dialect: dialect, Registry: reg, Admission: openLane{}, Owner: "metrics-test"}
	if _, err := refresher.RefreshDue(ctx); err != nil {
		t.Fatalf("refresh summaries: %v", err)
	}
	return Engine{DB: conn, Dialect: dialect, Catalog: NewCatalog(defs, []*doctype.DocType{dt})}
}

// consume delivers one sale's insert to the fact consumer in its own transaction.
func consume(ctx context.Context, t *testing.T, conn *sql.DB, b *facts.Builder, seq int64, s sale) {
	t.Helper()
	payload := map[string]any{
		"table": "Sale", "record_id": s.record,
		"data": map[string]any{
			"status": s.status, "total": s.total, "register": s.register, "legal_entity": s.entity,
			"items": []any{map[string]any{"name": s.record + "-L1", "item": "I1", "amount": s.total, "quantity": "1"}},
		},
	}
	event := contract.EventEnvelope{
		ID: fmt.Sprintf("evt-%d", seq), Type: outbox.EventTypeName("Sale", eventstream.EventInsert),
		Sequence: contract.Revision(strconv.FormatInt(seq, 10)), Site: "test",
		OccurredAt: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC),
		Data:       contract.MustEncodeData(payload),
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Consume(ctx, tx, event); err != nil {
		_ = tx.Rollback()
		t.Fatalf("consume %s: %v", s.record, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// expectation is one query and the rows a hand computation gives for it. wantPrevious is set
// only for a query that compares.
type expectation struct {
	name         string
	query        Query
	want         []string
	wantPrevious []string
}

// queries holds the hand computation. Sales is Paid totals; Receipts counts Paid sales; Average
// sale is Sales over Receipts, null when Receipts is zero. Day 18 Nov: Paid 100 (SAL-1) and 50
// (SAL-2), Return 30 (SAL-3) and Held 999 (SAL-4) excluded. Day 19 Nov: only Return 25 (SAL-6),
// so Sales and Receipts are zero and Average sale is null. Day 20 Nov: Paid 200 (SAL-5).
// Week of 16 Nov: 350 and 3.
func queries() []expectation {
	day := func(d int) tenanttime.Date { return tenanttime.Date{Year: 2026, Month: 11, Day: d} }
	all := []string{"sales", "receipts", "average_sale"}
	span := func(grain Grain, metrics []string) Query {
		return Query{Metrics: metrics, From: day(18), To: day(20), Grain: grain}
	}
	return []expectation{
		{"daily totals fill empty days with zero and a zero-receipt ratio with null", span(GrainDay, all), []string{
			"2026-11-18||150.00,2.00,75.00",
			"2026-11-19||0.00,0.00,null",
			"2026-11-20||200.00,1.00,200.00",
		}, nil},
		{"weekly bucket starts on Monday and divides totals", span(GrainWeek, all), []string{
			"2026-11-16||350.00,3.00,116.67",
		}, nil},
		{"monthly bucket", span(GrainMonth, all), []string{
			"2026-11-01||350.00,3.00,116.67",
		}, nil},
		// Range is one bucket keyed by 18 Nov: Paid 100+50+200 = 350 over 3 receipts. The previous
		// window is 15-17 Nov, three days, which holds SAL-7 (40, one receipt).
		{"range is one bucket keyed by its first day", span(GrainRange, all), []string{
			"2026-11-18||350.00,3.00,116.67",
		}, nil},
		{"range compares with as many days before it", Query{Metrics: all, From: day(18), To: day(20), Grain: GrainRange, Compare: ComparePreviousPeriod}, []string{
			"2026-11-18||350.00,3.00,116.67",
		}, []string{
			"2026-11-15||40.00,1.00,40.00",
		}},
		// R1 is 100 + 200 (SAL-3 and SAL-6 are returns); R2 is 50.
		{"range sums each group over the range", Query{Metrics: []string{"sales"}, Dimensions: []string{"register"}, From: day(18), To: day(20), Grain: GrainRange}, []string{
			"2026-11-18|R1|300.00",
			"2026-11-18|R2|50.00",
		}, nil},
		{"owner scope reads the facts", Query{Metrics: all, From: day(18), To: day(20), Grain: GrainDay, Scope: Scope{Owner: "u1"}}, []string{
			"2026-11-18||100.00,1.00,100.00",
			"2026-11-19||0.00,0.00,null",
			"2026-11-20||0.00,0.00,null",
		}, nil},
		{"entity scope reads the summary", Query{Metrics: all, From: day(18), To: day(20), Grain: GrainDay, Scope: Scope{LegalEntity: "E1"}}, []string{
			"2026-11-18||100.00,1.00,100.00",
			"2026-11-19||0.00,0.00,null",
			"2026-11-20||200.00,1.00,200.00",
		}, nil},
		{"one dimension groups the summary", Query{Metrics: []string{"sales"}, Dimensions: []string{"register"}, From: day(18), To: day(20), Grain: GrainDay}, []string{
			"2026-11-18|R1|100.00",
			"2026-11-19|R1|0.00",
			"2026-11-20|R1|200.00",
			"2026-11-18|R2|50.00",
			"2026-11-19|R2|0.00",
			"2026-11-20|R2|0.00",
		}, nil},
		// Previous period of 18-20 Nov is 15-17 Nov: SAL-7 (40, one receipt) is on 17 Nov, so it is
		// the last previous bucket, paired with 20 Nov.
		{"previous period pairs buckets by position", Query{Metrics: all, From: day(18), To: day(20), Grain: GrainDay, Compare: ComparePreviousPeriod}, []string{
			"2026-11-18||150.00,2.00,75.00",
			"2026-11-19||0.00,0.00,null",
			"2026-11-20||200.00,1.00,200.00",
		}, []string{
			"2026-11-15||0.00,0.00,null",
			"2026-11-16||0.00,0.00,null",
			"2026-11-17||40.00,1.00,40.00",
		}},
		// Register totals over 17-20 Nov: R1 300, R2 50, R3 40. Top two keep R1 and R2; R3 folds into Other.
		{"top two folds the rest into Other", Query{Metrics: []string{"sales"}, Dimensions: []string{"register"}, From: day(17), To: day(20), Grain: GrainDay, Top: 2}, []string{
			"2026-11-17|R1|0.00",
			"2026-11-18|R1|100.00",
			"2026-11-19|R1|0.00",
			"2026-11-20|R1|200.00",
			"2026-11-17|R2|0.00",
			"2026-11-18|R2|50.00",
			"2026-11-19|R2|0.00",
			"2026-11-20|R2|0.00",
			"2026-11-17|__other__|40.00",
			"2026-11-18|__other__|0.00",
			"2026-11-19|__other__|0.00",
			"2026-11-20|__other__|0.00",
		}, nil},
		// Net sales is sales minus refunds per day: 18 Nov 150 - 30, 19 Nov 0 - 25, 20 Nov 200 - 0.
		// Average sale is null on 19 Nov, so the derived metric is null only when it reads it.
		{"derived metric over simple metrics", Query{Metrics: []string{"net_sales", "average_sale"}, From: day(18), To: day(20), Grain: GrainDay}, []string{
			"2026-11-18||120.00,75.00",
			"2026-11-19||-25.00,null",
			"2026-11-20||200.00,200.00",
		}, nil},
		// Sales to date: 18 Nov 150, 19 Nov nothing new, 20 Nov 200 more.
		{"cumulative starts at zero on the first day", Query{Metrics: []string{"sales_to_date"}, From: day(18), To: day(20), Grain: GrainDay}, []string{
			"2026-11-18||150.00",
			"2026-11-19||150.00",
			"2026-11-20||350.00",
		}, nil},
	}
}

// render prints each row as period|group|values with two decimals, or null, one line per row.
func render(rows []Row) []string {
	var out []string
	for _, r := range rows {
		vals := make([]string, len(r.Values))
		for i, v := range r.Values {
			vals[i] = "null"
			if v.Valid {
				vals[i] = v.Decimal.StringFixed(2)
			}
		}
		out = append(out, r.Period.String()+"|"+strings.Join(r.Group, "/")+"|"+strings.Join(vals, ","))
	}
	return out
}
