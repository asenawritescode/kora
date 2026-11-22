//go:build integration

package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/eventstream"
	"github.com/asenawritescode/kora/internal/facts"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// posLine is one Sale Item line of a POS sale.
type posLine struct{ item, quantity, amount string }

// posSale is one POS sale as the fact builder reads it. Created is UTC; the business date is the Nairobi date.
type posSale struct {
	name, status, register, created, total string
	lines                                  []posLine
}

// posSales: POS-1 and POS-2 fall on 18 November, POS-3 and POS-4 on 19 November. POS-4 is held, so it has no facts.
// Paid lines: items_sold is 2+1+3+4+1 = 11, of register R1 2+1+4+1 = 8, of item I1 2+3+1 = 6.
// Sales are 250+300+180 = 730, of R1 250+180 = 430.
var posSales = []posSale{
	{"POS-1", "Paid", "R1", "2026-11-18 09:00:00", "250", []posLine{{"I1", "2", "200"}, {"I2", "1", "50"}}},
	{"POS-2", "Paid", "R2", "2026-11-18 10:00:00", "300", []posLine{{"I1", "3", "300"}}},
	{"POS-3", "Paid", "R1", "2026-11-19 09:00:00", "180", []posLine{{"I2", "4", "80"}, {"I1", "1", "100"}}},
	{"POS-4", "Held", "R1", "2026-11-19 11:00:00", "999", []posLine{{"I1", "5", "999"}}},
}

// summarisedSales is how many leading sales are refreshed into summaries; the rest stay dirty, so the
// 18 November days read summaries and the 19 November days read fact rows.
const summarisedSales = 2

// TestDrillTotalMatchesMetricAcrossShapes drills POS sales over 18 to 19 November, with a summarised day and a
// dirty day. For each shape the drill total must equal the metric value, and the listed records must sum to it.
func TestDrillTotalMatchesMetricAcrossShapes(t *testing.T) {
	shapes := []struct {
		name, metric string
		dims, vals   []string
		want         string
	}{
		{"header metric, ungrouped", "sales", nil, nil, "730"},
		{"header metric, grouped by a header dimension", "sales", []string{"register"}, []string{"R1"}, "430"},
		{"line sum, grouped by a line dimension", "items_sold", []string{"items.item"}, []string{"I1"}, "6"},
		{"line sum, ungrouped", "items_sold", nil, nil, "11"},
		{"line sum, grouped by a header dimension", "items_sold", []string{"register"}, []string{"R1"}, "8"},
	}
	dialects := []struct {
		driver, dialect, envVar string
	}{
		{"mysql", "mysql", "KORA_METRICS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_METRICS_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range dialects {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run", tc.envVar)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			engine := seedPOSSales(t, database.DB, db.Resolve(tc.dialect))
			for _, s := range shapes {
				t.Run(s.name, func(t *testing.T) {
					checkDrillShape(t, engine, s.metric, s.dims, s.vals, s.want)
				})
			}
		})
	}
}

// checkDrillShape compares the metric value for one group with the drill total for the same group, and sums the
// drill's records to check they add up to the total.
func checkDrillShape(t *testing.T, engine Engine, metric string, dims, vals []string, want string) {
	t.Helper()
	ctx := context.Background()
	from := tenanttime.Date{Year: 2026, Month: 11, Day: 18}
	to := tenanttime.Date{Year: 2026, Month: 11, Day: 19}
	res, err := engine.Query(ctx, Query{Metrics: []string{metric}, Dimensions: dims, From: from, To: to, Grain: GrainRange})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	value := metricValue(t, res.Rows, vals)
	if !value.Equal(decimal.RequireFromString(want)) {
		t.Fatalf("metric = %s, want %s", value, want)
	}
	group := map[string]string{}
	for i, d := range dims {
		group[d] = vals[i]
	}
	clock := tenanttime.Clock{Location: mustNairobi(t)}
	page, err := engine.DrillRecords(ctx, DrillRequest{Metric: metric, From: from, To: to, Group: group}, Scope{}, clock, "")
	if err != nil {
		t.Fatalf("drill: %v", err)
	}
	if !page.Total.Equal(value) {
		t.Errorf("drill total = %s, metric = %s", page.Total, value)
	}
	sum := decimal.Zero
	for _, r := range page.Records {
		sum = sum.Add(r.Value)
	}
	if !sum.Equal(page.Total) {
		t.Errorf("records sum to %s, drill total is %s", sum, page.Total)
	}
}

// metricValue returns the value of the row for one group; an ungrouped query has one row with no group values.
func metricValue(t *testing.T, rows []Row, group []string) decimal.Decimal {
	t.Helper()
	for _, r := range rows {
		if slices.Equal(r.Group, group) {
			return r.Values[0].Decimal
		}
	}
	t.Fatalf("no row for group %v in %d rows", group, len(rows))
	return decimal.Decimal{}
}

// seedPOSSales creates the sales table and the site time zone, writes the POS sales through the fact builder
// and refreshes the summaries of the first summarisedSales sales only.
func seedPOSSales(t *testing.T, conn *sql.DB, dialect db.Dialect) Engine {
	t.Helper()
	ctx := context.Background()
	q := dialect.QuoteIdent
	setup := []string{
		fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, owner VARCHAR(140) NOT NULL, creation TIMESTAMP NOT NULL, status VARCHAR(40) NOT NULL, total DECIMAL(18,2) NOT NULL, register VARCHAR(40) NOT NULL)", q("tabSale")),
		"CREATE TABLE _kora_site_setting (site VARCHAR(140) NOT NULL, setting_key VARCHAR(140) NOT NULL, setting_value VARCHAR(140) NOT NULL, PRIMARY KEY (site, setting_key))",
	}
	for _, stmt := range setup {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	zone := db.Rebind(dialect, "INSERT INTO _kora_site_setting (site, setting_key, setting_value) VALUES (?, ?, ?)")
	if _, err := conn.ExecContext(ctx, zone, "test", tenanttime.KeyTimeZone, "Africa/Nairobi"); err != nil {
		t.Fatalf("site zone: %v", err)
	}
	dts := posDoctypes(t)
	reg := doctype.NewRegistry()
	for _, dt := range dts {
		reg.Register(dt)
	}
	builder := &facts.Builder{DB: conn, Dialect: dialect, Registry: reg}
	refresher := &facts.Refresher{DB: conn, Dialect: dialect, Registry: reg, Admission: openLane{}, Owner: "drill-shapes-test"}
	insert := db.Rebind(dialect, fmt.Sprintf("INSERT INTO %s (name, owner, creation, status, total, register) VALUES (?, ?, ?, ?, ?, ?)", q("tabSale")))
	for i, s := range posSales {
		if _, err := conn.ExecContext(ctx, insert, s.name, "u1", s.created, s.status, s.total, s.register); err != nil {
			t.Fatalf("insert %s: %v", s.name, err)
		}
		consumePOSSale(ctx, t, conn, builder, int64(i+1), s)
		if i+1 == summarisedSales {
			if _, err := refresher.RefreshDue(ctx); err != nil {
				t.Fatalf("refresh summaries: %v", err)
			}
		}
	}
	return Engine{DB: conn, Dialect: dialect, Catalog: NewCatalog(posMetricDefs(t), dts)}
}

// consumePOSSale delivers one sale's insert, with its lines, to the fact builder in its own transaction.
func consumePOSSale(ctx context.Context, t *testing.T, conn *sql.DB, b *facts.Builder, seq int64, s posSale) {
	t.Helper()
	var lines []any
	for i, l := range s.lines {
		lines = append(lines, map[string]any{"name": fmt.Sprintf("%s-L%d", s.name, i+1), "item": l.item, "quantity": l.quantity, "amount": l.amount})
	}
	payload := map[string]any{
		"table": "Sale", "record_id": s.name,
		"data": map[string]any{"status": s.status, "total": s.total, "register": s.register, "items": lines},
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
		t.Fatalf("consume %s: %v", s.name, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
