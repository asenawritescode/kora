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

// adjustmentMovement is one Stock Movement with the reason and the record that caused it.
type adjustmentMovement struct {
	record, product, created, change, reason, source string
}

// seedAdjustments: two adjustments (a damage on 18 Nov and a count correction on 19 Nov) and one sale.
// The stock_adjusted metric reads only the adjustments, grouped by reason.
var seedAdjustments = []adjustmentMovement{
	{"ADJ-1", "P1", "2026-11-18 12:00:00", "-2", "Damaged", "Stock Adjustment"},
	{"ADJ-2", "P2", "2026-11-19 12:00:00", "3", "Count correction", "Stock Adjustment"},
	{"MOV-9", "P1", "2026-11-18 13:00:00", "-1", "Sale", "Sale"},
}

// TestStockAdjustedByReasonAcrossDialects runs the erpsell stock_adjusted metric by reason, the query
// behind the Stock adjustments table of Shop insights, over seeded stock movements.
func TestStockAdjustedByReasonAcrossDialects(t *testing.T) {
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
			engine := seedAdjustmentEngine(t, database.DB, db.Resolve(tc.dialect))
			scopes := map[string]Scope{
				"tenant":      {},
				"owner":       {Owner: "wh1"},
				"owner+joins": {Owner: "wh1", Joinable: map[string]bool{"Stock Movement": true}},
			}
			for name, scope := range scopes {
				for _, grain := range []Grain{GrainRange, GrainDay} {
					res, err := engine.Query(context.Background(), Query{Metrics: []string{"stock_adjusted"}, Dimensions: []string{"reason"},
						From: tenanttime.Date{Year: 2026, Month: 11, Day: 18}, To: tenanttime.Date{Year: 2026, Month: 11, Day: 20}, Grain: grain, Scope: scope})
					if err != nil {
						t.Fatalf("%s grain %s: query: %v", name, grain, err)
					}
					if len(res.Rows) == 0 {
						t.Fatalf("%s grain %s: no rows", name, grain)
					}
				}
			}
			// The query endpoint also reads the freshness and rebuild state of the models it reads.
			stock := engine.Catalog.Models["Stock Movement"]
			if _, err := engine.SourceSequence(context.Background(), []*doctype.DocType{stock}); err != nil {
				t.Fatalf("source sequence: %v", err)
			}
			store := &facts.JobStore{DB: database.DB, Dialect: engine.Dialect}
			// Claim creates the job table, as the server's startup does, before Progress reads it.
			if job, err := store.Claim(context.Background(), "none", "metrics-test"); err != nil || job != nil {
				t.Fatalf("job table: claim = %v, %v", job, err)
			}
			if _, err := store.Progress(context.Background(), []string{"Stock Movement"}); err != nil {
				t.Fatalf("rebuild progress: %v", err)
			}
		})
	}
}

// seedAdjustmentEngine creates the Stock Movement table and facts, consumes the adjustments through the
// fact consumer, refreshes the summaries and returns an engine over the erpsell metrics.
func seedAdjustmentEngine(t *testing.T, conn *sql.DB, dialect db.Dialect) Engine {
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
	defs, err := ParseDirectory("../../config/_parked/erpsell/metrics")
	if err != nil {
		t.Fatalf("parse selling metrics: %v", err)
	}
	reg := doctype.NewRegistry()
	reg.Register(stock)
	builder := &facts.Builder{DB: conn, Dialect: dialect, Registry: reg}
	for i, m := range seedAdjustments {
		insert := db.Rebind(dialect, fmt.Sprintf("INSERT INTO %s (name, owner, creation) VALUES (?, ?, ?)", q("tabStock Movement")))
		if _, err := conn.ExecContext(ctx, insert, m.record, "wh1", m.created); err != nil {
			t.Fatalf("insert %s: %v", m.record, err)
		}
		consumeAdjustment(ctx, t, conn, builder, int64(i+1), m)
	}
	refresher := &facts.Refresher{DB: conn, Dialect: dialect, Registry: reg, Admission: openLane{}, Owner: "metrics-test"}
	if _, err := refresher.RefreshDue(ctx); err != nil {
		t.Fatalf("refresh summaries: %v", err)
	}
	return Engine{DB: conn, Dialect: dialect, Catalog: NewCatalog(defs, []*doctype.DocType{stock})}
}

// consumeAdjustment delivers one movement's insert to the fact consumer in its own transaction.
func consumeAdjustment(ctx context.Context, t *testing.T, conn *sql.DB, b *facts.Builder, seq int64, m adjustmentMovement) {
	t.Helper()
	payload := map[string]any{
		"table": "Stock Movement", "record_id": m.record,
		"data": map[string]any{"product": m.product, "quantity_change": m.change, "reason": m.reason, "source_doctype": m.source},
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
