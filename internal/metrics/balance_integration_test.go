//go:build integration

package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"os"
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
	"strconv"
)

// movement is one Stock Movement as the seed writes it. Every movement is owned by wh1.
type movement struct {
	record, product, created, change string
}

// seedMovements: P1 starts with 10 on 10 Nov and loses 4 on 15 Nov, so 6 is the opening of 18 Nov.
// P2 gets 6 on 15 Nov, so it has no movement on 18 Nov. P3 gets 2 on 12 Nov and nothing in the
// range, so it is a group that exists only through its opening. Inside 18-20 Nov, P1 sells 3 on
// 18 Nov and receives 5 on 20 Nov; P2 loses 2 on 19 Nov and receives 3 on 20 Nov.
var seedMovements = []movement{
	{"MOV-1", "P1", "2026-11-10 12:00:00", "10"},
	{"MOV-2", "P3", "2026-11-12 12:00:00", "2"},
	{"MOV-3", "P1", "2026-11-15 12:00:00", "-4"},
	{"MOV-4", "P2", "2026-11-15 12:00:00", "6"},
	{"MOV-5", "P1", "2026-11-18 12:00:00", "-3"},
	{"MOV-6", "P2", "2026-11-19 12:00:00", "-2"},
	{"MOV-7", "P1", "2026-11-20 12:00:00", "5"},
	{"MOV-8", "P2", "2026-11-20 12:00:00", "3"},
}

// TestStockBalanceAcrossDialects seeds stock movements through the fact consumer and checks the
// balance, cumulative and derived metrics over them against a hand computation. Each query runs
// on the summary when it can and on the facts when it is owner-scoped, so both opening paths run.
func TestStockBalanceAcrossDialects(t *testing.T) {
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
			engine := seedStock(t, database.DB, db.Resolve(tc.dialect))
			for _, q := range stockQueries() {
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

// seedStock creates the Stock Movement records and their facts, refreshes the summaries and returns
// an engine over the seeded database with the test-only metrics.
func seedStock(t *testing.T, conn *sql.DB, dialect db.Dialect) Engine {
	t.Helper()
	ctx := context.Background()
	q := dialect.QuoteIdent
	setup := []string{
		fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, owner VARCHAR(140) NOT NULL, creation TIMESTAMP NOT NULL)", q("tabStock Movement")),
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
	dts, err := doctype.ParseConfigTree("../../config/_parked/erpsell")
	if err != nil {
		t.Fatalf("parse selling pack: %v", err)
	}
	var stock *doctype.DocType
	for _, dt := range dts {
		if dt.Name == "Stock Movement" {
			stock = dt
		}
	}
	if stock == nil || stock.Facts == nil {
		t.Fatal("selling pack has no Stock Movement fact model")
	}
	defs := []Metric{
		{Name: "stock_on_hand", Label: "Stock on hand", Type: KindBalance, Model: "Stock Movement", Measure: &Measure{Agg: aggSum, Field: "quantity_change"}},
		{Name: "stock_change_to_date", Label: "Stock change", Type: KindCumulative, Model: "Stock Movement", Measure: &Measure{Agg: aggSum, Field: "quantity_change"}},
		{Name: "on_hand_twice", Label: "On hand twice", Type: KindDerived, Formula: "stock_on_hand * 2"},
	}
	if err := Validate(defs, []*doctype.DocType{stock}); err != nil {
		t.Fatalf("metrics do not validate: %v", err)
	}
	reg := doctype.NewRegistry()
	reg.Register(stock)
	builder := &facts.Builder{DB: conn, Dialect: dialect, Registry: reg}
	for i, m := range seedMovements {
		insert := db.Rebind(dialect, fmt.Sprintf("INSERT INTO %s (name, owner, creation) VALUES (?, ?, ?)", q("tabStock Movement")))
		if _, err := conn.ExecContext(ctx, insert, m.record, "wh1", m.created); err != nil {
			t.Fatalf("insert %s: %v", m.record, err)
		}
		consumeMovement(ctx, t, conn, builder, int64(i+1), m)
	}
	refresher := &facts.Refresher{DB: conn, Dialect: dialect, Registry: reg, Admission: openLane{}, Owner: "metrics-test"}
	if _, err := refresher.RefreshDue(ctx); err != nil {
		t.Fatalf("refresh summaries: %v", err)
	}
	return Engine{DB: conn, Dialect: dialect, Catalog: NewCatalog(defs, []*doctype.DocType{stock})}
}

// consumeMovement delivers one movement's insert to the fact consumer in its own transaction.
func consumeMovement(ctx context.Context, t *testing.T, conn *sql.DB, b *facts.Builder, seq int64, m movement) {
	t.Helper()
	payload := map[string]any{
		"table": "Stock Movement", "record_id": m.record,
		"data": map[string]any{"product": m.product, "quantity_change": m.change, "reason": "Receipt", "source_doctype": "Sale"},
	}
	event := contract.EventEnvelope{
		ID: fmt.Sprintf("evt-%d", seq), Type: outbox.EventTypeName("Stock Movement", eventstream.EventInsert),
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
		t.Fatalf("consume %s: %v", m.record, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// stockQueries holds the hand computation. Stock on hand by product: P1 6 on 18 Nov before the
// range, then 3 after the sale, then 8 after the receipt on 20 Nov; P2 6, 4, 7; P3 2 throughout.
// The total is 11, 9, 17. Previous period 15-17 Nov: P1 6 (after its loss of 4 on 15 Nov), P2 6, P3 2.
func stockQueries() []expectation {
	day := func(d int) tenanttime.Date { return tenanttime.Date{Year: 2026, Month: 11, Day: d} }
	byProduct := func(from, to int) Query {
		return Query{Metrics: []string{"stock_on_hand"}, Dimensions: []string{"product"}, From: day(from), To: day(to), Grain: GrainDay}
	}
	onHandRows := []string{
		"2026-11-18|P1|3.00", "2026-11-19|P1|3.00", "2026-11-20|P1|8.00",
		"2026-11-18|P2|6.00", "2026-11-19|P2|4.00", "2026-11-20|P2|7.00",
		"2026-11-18|P3|2.00", "2026-11-19|P3|2.00", "2026-11-20|P3|2.00",
	}
	ownerScoped := byProduct(18, 20)
	ownerScoped.Scope = Scope{Owner: "wh1"}
	withPrevious := byProduct(18, 20)
	withPrevious.Compare = ComparePreviousPeriod
	return []expectation{
		{"balance by product opens from the days before the range", byProduct(18, 20), onHandRows, nil},
		{"owner-scoped balance reads the facts and opens the same", ownerScoped, onHandRows, nil},
		{"balance total without a dimension", Query{Metrics: []string{"stock_on_hand"}, From: day(18), To: day(20), Grain: GrainDay}, []string{
			"2026-11-18||11.00",
			"2026-11-19||9.00",
			"2026-11-20||17.00",
		}, nil},
		{"weekly balance is the value at the range end", Query{Metrics: []string{"stock_on_hand"}, From: day(18), To: day(20), Grain: GrainWeek}, []string{
			"2026-11-16||17.00",
		}, nil},
		{"previous period opens from before its own range", withPrevious, onHandRows, []string{
			"2026-11-15|P1|6.00", "2026-11-16|P1|6.00", "2026-11-17|P1|6.00",
			"2026-11-15|P2|6.00", "2026-11-16|P2|6.00", "2026-11-17|P2|6.00",
			"2026-11-15|P3|2.00", "2026-11-16|P3|2.00", "2026-11-17|P3|2.00",
		}},
		// A range is one bucket keyed by 18 Nov, holding the balance as of 20 Nov: P1 8, P2 7, P3 2, total 17.
		{"range balance by product is the value at the range end", Query{Metrics: []string{"stock_on_hand"}, Dimensions: []string{"product"}, From: day(18), To: day(20), Grain: GrainRange}, []string{
			"2026-11-18|P1|8.00",
			"2026-11-18|P2|7.00",
			"2026-11-18|P3|2.00",
		}, nil},
		{"range balance total", Query{Metrics: []string{"stock_on_hand"}, From: day(18), To: day(20), Grain: GrainRange}, []string{
			"2026-11-18||17.00",
		}, nil},
		// The previous window is 15-17 Nov: balances as of 17 Nov, P1 6, P2 6, P3 2, keyed by 15 Nov.
		{"range compare opens the previous window from before it", Query{Metrics: []string{"stock_on_hand"}, Dimensions: []string{"product"}, From: day(18), To: day(20), Grain: GrainRange, Compare: ComparePreviousPeriod}, []string{
			"2026-11-18|P1|8.00",
			"2026-11-18|P2|7.00",
			"2026-11-18|P3|2.00",
		}, []string{
			"2026-11-15|P1|6.00",
			"2026-11-15|P2|6.00",
			"2026-11-15|P3|2.00",
		}},
		// Cumulative change over the range: P1 -3 then +5, P2 -2 then +3; P3 has no movement in range.
		{"range cumulative change sums movements inside the range", Query{Metrics: []string{"stock_change_to_date"}, Dimensions: []string{"product"}, From: day(18), To: day(20), Grain: GrainRange}, []string{
			"2026-11-18|P1|2.00",
			"2026-11-18|P2|1.00",
		}, nil},
		// Last-bucket ranking keeps P1 (8) over P2 (7); summing buckets would keep P2 (17) over P1 (14).
		{"top one ranks a balance by its last bucket and folds the rest", func() Query {
			q := byProduct(18, 20)
			q.Top = 1
			return q
		}(), []string{
			"2026-11-18|P1|3.00", "2026-11-19|P1|3.00", "2026-11-20|P1|8.00",
			"2026-11-18|__other__|8.00", "2026-11-19|__other__|6.00", "2026-11-20|__other__|9.00",
		}, nil},
		// Cumulative change counts movements inside the range only, so P3 has no row.
		{"cumulative change starts at zero inside the range", Query{Metrics: []string{"stock_change_to_date"}, Dimensions: []string{"product"}, From: day(18), To: day(20), Grain: GrainDay}, []string{
			"2026-11-18|P1|-3.00", "2026-11-19|P1|-3.00", "2026-11-20|P1|2.00",
			"2026-11-18|P2|0.00", "2026-11-19|P2|-2.00", "2026-11-20|P2|1.00",
		}, nil},
		{"derived over a balance works per bucket", Query{Metrics: []string{"on_hand_twice"}, Dimensions: []string{"product"}, From: day(18), To: day(20), Grain: GrainDay}, []string{
			"2026-11-18|P1|6.00", "2026-11-19|P1|6.00", "2026-11-20|P1|16.00",
			"2026-11-18|P2|12.00", "2026-11-19|P2|8.00", "2026-11-20|P2|14.00",
			"2026-11-18|P3|4.00", "2026-11-19|P3|4.00", "2026-11-20|P3|4.00",
		}, nil},
	}
}
