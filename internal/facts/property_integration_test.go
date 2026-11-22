//go:build integration

package facts

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"slices"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/eventstream"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// TestSummariesEqualSourceAfterRandomStreams sends random inserts, updates (status changes and backdates),
// deletes and redeliveries through the consumer and the refresher. After a final refresh, every day's
// summary must equal the aggregate of the source records mapped by facts.Map, and the live fact headers
// must match the live source records.
func TestSummariesEqualSourceAfterRandomStreams(t *testing.T) {
	for _, tc := range []struct{ driver, envVar string }{
		{"mysql", "KORA_FACTS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_FACTS_DISPOSABLE_POSTGRES_DSN"},
	} {
		for _, seed := range []uint64{1, 2, 3} {
			t.Run(fmt.Sprintf("%s/seed%d", tc.driver, seed), func(t *testing.T) {
				dsn := os.Getenv(tc.envVar)
				if dsn == "" {
					t.Skipf("set %s to run", tc.envVar)
				}
				database := disposabledb.Open(t, tc.driver, dsn)
				dialect := db.Resolve(tc.driver)
				ctx := context.Background()
				setupEmptySales(t, database.DB, dialect)
				held := saleFacts()
				held.Facts.ExcludeStatus = []string{"Held"}
				reg := doctype.NewRegistry()
				reg.Register(held)
				builder := &Builder{DB: database.DB, Dialect: dialect, Registry: reg}
				refresher := &Refresher{DB: database.DB, Dialect: dialect, Registry: reg, Admission: openLane{}, Owner: "test-refresher"}

				rnd := rand.New(rand.NewPCG(seed, 7))
				source := map[string]map[string]any{}
				var events []contract.EventEnvelope
				var seq int64
				next := 0
				send := func(ev contract.EventEnvelope) {
					t.Helper()
					consumeEnvelope(t, ctx, database.DB, builder, ev)
				}
				for step := 0; step < 120; step++ {
					names := slices.Sorted(maps.Keys(source))
					switch op := rnd.IntN(10); {
					case op < 4 || len(names) == 0:
						next++
						name := fmt.Sprintf("SAL-%03d", next)
						data := randomSale(rnd, name)
						source[name] = data
						seq++
						events = append(events, saleEventFor(name, seq, eventstream.EventInsert, data, nil))
					case op < 7:
						name := names[rnd.IntN(len(names))]
						old := source[name]
						data := randomSale(rnd, name)
						source[name] = data
						seq++
						events = append(events, saleEventFor(name, seq, eventstream.EventUpdate, data, old))
					case op < 8:
						name := names[rnd.IntN(len(names))]
						old := source[name]
						delete(source, name)
						seq++
						events = append(events, saleEventFor(name, seq, eventstream.EventDelete, nil, old))
					default:
						// Redelivery of an earlier event must change nothing.
						send(events[rnd.IntN(len(events))])
						continue
					}
					send(events[len(events)-1])
					if step%15 == 14 {
						if _, err := refresher.RefreshDue(ctx); err != nil {
							t.Fatal(err)
						}
					}
				}
				if _, err := refresher.RefreshDue(ctx); err != nil {
					t.Fatal(err)
				}

				var rows []Row
				for name, data := range source {
					head, lines, err := Map(held, Change{RecordID: name, Sequence: seq, Data: data}, tenanttime.Clock{})
					if err != nil {
						t.Fatalf("map: %v", err)
					}
					rows = append(rows, head)
					rows = append(rows, lines...)
				}
				byDay := map[string][]Row{}
				for _, r := range rows {
					byDay[r.BusinessDate.String()] = append(byDay[r.BusinessDate.String()], r)
				}
				if len(source) < 5 || len(events) < 60 {
					t.Fatalf("stream too small to test anything: %d records, %d events", len(source), len(events))
				}
				for day := 14; day <= 18; day++ {
					d := fmt.Sprintf("2026-11-%02d", day)
					if got, want := summaryOf(t, database.DB, dialect, d), expectedSummary(byDay[d]); !maps.Equal(got, want) {
						t.Fatalf("day %s: summary = %v\nwant %v", d, got, want)
					}
					live := 0
					for _, r := range byDay[d] {
						if !r.Deleted && r.LineID == "" {
							live++
						}
					}
					if n := countRows(t, database.DB, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(factHeader)+" WHERE line_id = '' AND deleted = ? AND business_date = ?", db.BoolParam(dialect, false), d); n != live {
						t.Fatalf("day %s: live fact headers = %d, source says %d", d, n, live)
					}
				}
			})
		}
	}
}

// expectedSummary aggregates live rows the way the summary is defined: per status, a day total (grain "")
// and one grain per dimension, counting header rows and summing measures over every row.
func expectedSummary(rows []Row) map[string]string {
	type acc struct {
		count       int
		total, line decimal.Decimal
	}
	sums := map[string]*acc{}
	add := func(status, grain, value string, r Row) {
		key := status + "/" + grain + "/" + value + "/" + r.LegalEntity
		a := sums[key]
		if a == nil {
			a = &acc{total: decimal.Zero, line: decimal.Zero}
			sums[key] = a
		}
		if r.LineID == "" {
			a.count++
		}
		a.total = a.total.Add(r.Measures["total"])
		a.line = a.line.Add(r.Measures["line_amount"])
	}
	for _, r := range rows {
		if r.Deleted {
			continue
		}
		add(r.Status, "", "", r)
		add(r.Status, "register", r.Dimensions["register"], r)
		add(r.Status, "item", r.Dimensions["item"], r)
	}
	out := map[string]string{}
	for key, a := range sums {
		out[key] = fmt.Sprintf("%d|%s|%s", a.count, a.total.String(), a.line.String())
	}
	return out
}

// randomSale is a Sale payload with a random status (Held among them), day, register and lines.
func randomSale(rnd *rand.Rand, name string) map[string]any {
	statuses := []string{"Draft", "Paid", "Void", "Return", "Held"}
	var items []any
	for i := range rnd.IntN(3) {
		items = append(items, line(fmt.Sprintf("%s-L%d", name, i), fmt.Sprintf("I%d", 1+rnd.IntN(3)), fmt.Sprintf("%d.%02d", rnd.IntN(90), rnd.IntN(100))))
	}
	return map[string]any{
		"posting_date": fmt.Sprintf("2026-11-%02d", 14+rnd.IntN(5)),
		"total":        fmt.Sprintf("%d.%02d", rnd.IntN(500), rnd.IntN(100)),
		"status":       statuses[rnd.IntN(len(statuses))],
		"register":     fmt.Sprintf("R%d", 1+rnd.IntN(2)),
		"items":        items,
	}
}

// consumeEnvelope delivers one event to the builder in its own transaction.
func consumeEnvelope(t *testing.T, ctx context.Context, conn *sql.DB, builder *Builder, ev contract.EventEnvelope) {
	t.Helper()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = builder.Consume(ctx, tx, ev); err != nil {
		_ = tx.Rollback()
		t.Fatalf("consume seq %s: %v", ev.Sequence, err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// setupEmptySales creates the Sale tables the consumer reads from, with no rows: the payload supplies them.
func setupEmptySales(t *testing.T, conn *sql.DB, dialect db.Dialect) {
	t.Helper()
	for _, stmt := range []string{
		fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, owner VARCHAR(140) NOT NULL, creation TIMESTAMP NOT NULL)", dialect.QuoteIdent("tabSale")),
		"CREATE TABLE _kora_site_setting (site VARCHAR(140) NOT NULL, setting_key VARCHAR(140) NOT NULL, setting_value VARCHAR(140) NOT NULL, PRIMARY KEY (site, setting_key))",
	} {
		if _, err := conn.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
}
