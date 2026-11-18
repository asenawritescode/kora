//go:build integration

package facts

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/eventstream"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// fullLane rejects every maintenance unit, as a saturated lane does.
type fullLane struct{}

func (fullLane) AcquireMaintenance(context.Context) (func(), error) {
	return nil, dbaccess.ErrExhausted
}

// openLane admits every maintenance unit.
type openLane struct{}

func (openLane) AcquireMaintenance(context.Context) (func(), error) { return func() {}, nil }

// TestSummariesFollowFactChanges drives one Sale through the real consumer (insert, edit, backdate,
// delete, re-insert) and checks each refreshed day against the fact tables and a hand-derived
// expectation. It then checks that a second refresh is a no-op and that a full lane defers work.
func TestSummariesFollowFactChanges(t *testing.T) {
	tests := []struct {
		driver, dialect, envVar string
	}{
		{"mysql", "mysql", "KORA_FACTS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_FACTS_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range tests {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run", tc.envVar)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			dialect := db.Resolve(tc.dialect)
			ctx := context.Background()
			for _, stmt := range []string{
				fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, owner VARCHAR(140) NOT NULL, creation TIMESTAMP NOT NULL)", dialect.QuoteIdent("tabSale")),
				"CREATE TABLE _kora_site_setting (site VARCHAR(140) NOT NULL, setting_key VARCHAR(140) NOT NULL, setting_value VARCHAR(140) NOT NULL, PRIMARY KEY (site, setting_key))",
				db.Rebind(dialect, fmt.Sprintf("INSERT INTO %s (name, owner, creation) VALUES (?, ?, ?)", dialect.QuoteIdent("tabSale"))),
			} {
				args := []any{}
				if strings.HasPrefix(stmt, "INSERT") {
					args = []any{"SAL-1", "u1", "2026-11-01 10:00:00"}
				}
				if _, err := database.DB.ExecContext(ctx, stmt, args...); err != nil {
					t.Fatalf("setup %q: %v", stmt, err)
				}
			}
			reg := doctype.NewRegistry()
			reg.Register(saleFacts())
			builder := &Builder{DB: database.DB, Dialect: dialect, Registry: reg}
			refresher := &Refresher{DB: database.DB, Dialect: dialect, Registry: reg, Admission: openLane{}, Owner: "test-refresher"}
			consume := func(seq int64, op eventstream.EventOp, data, old map[string]any) {
				t.Helper()
				tx, err := database.DB.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := builder.Consume(ctx, tx, saleEvent(seq, op, data, old)); err != nil {
					_ = tx.Rollback()
					t.Fatalf("consume seq %d: %v", seq, err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			refresh := func(t *testing.T, r *Refresher, want int) {
				t.Helper()
				n, err := r.RefreshDue(ctx)
				if err != nil || n != want {
					t.Fatalf("RefreshDue = %d, %v; want %d, nil", n, err, want)
				}
			}
			expect := func(t *testing.T, day string, want map[string]string) {
				t.Helper()
				got := summaryOf(t, database.DB, dialect, day)
				if !maps.Equal(got, want) {
					t.Fatalf("summary %s = %v, want %v", day, got, want)
				}
				assertMatchesFacts(t, database.DB, dialect, day, got)
			}

			v1 := sale("2026-11-18", line("L1", "I1", "40.25"), line("L2", "I2", "60.25"))
			consume(1, eventstream.EventInsert, v1, nil)
			refresh(t, refresher, 1)
			expect(t, "2026-11-18", map[string]string{
				"//le1": "1|100.5|100.5", "register/R1/le1": "1|100.5|100.5", "item//le1": "1|100.5|0",
				"item/I1/le1": "0|0|40.25", "item/I2/le1": "0|0|60.25",
			})

			v2 := sale("2026-11-18", line("L1", "I1", "40.25"), line("L2", "I2", "60.25"))
			v2["total"] = "120"
			consume(2, eventstream.EventUpdate, v2, v1)
			refresh(t, refresher, 1)
			expect(t, "2026-11-18", map[string]string{
				"//le1": "1|120|100.5", "register/R1/le1": "1|120|100.5", "item//le1": "1|120|0",
				"item/I1/le1": "0|0|40.25", "item/I2/le1": "0|0|60.25",
			})

			v3 := sale("2026-11-17", line("L1", "I1", "40.25"), line("L2", "I2", "60.25"))
			v3["total"] = "120"
			consume(3, eventstream.EventUpdate, v3, v2)
			refresh(t, refresher, 2)
			expect(t, "2026-11-18", map[string]string{})
			expect(t, "2026-11-17", map[string]string{
				"//le1": "1|120|100.5", "register/R1/le1": "1|120|100.5", "item//le1": "1|120|0",
				"item/I1/le1": "0|0|40.25", "item/I2/le1": "0|0|60.25",
			})

			consume(4, eventstream.EventDelete, nil, v3)
			refresh(t, refresher, 1)
			expect(t, "2026-11-17", map[string]string{})

			refresh(t, refresher, 0)
			expect(t, "2026-11-17", map[string]string{})
			if n := countDirty(t, database.DB, dialect); n != 0 {
				t.Fatalf("%d dirty markers left after refresh, want 0", n)
			}

			v5 := sale("2026-11-18")
			v5["total"] = "50"
			consume(5, eventstream.EventInsert, v5, nil)
			deferred := &Refresher{DB: database.DB, Dialect: dialect, Registry: reg, Admission: fullLane{}, Owner: "test-refresher"}
			refresh(t, deferred, 0)
			if n := countDirty(t, database.DB, dialect); n != 1 {
				t.Fatalf("%d dirty markers after deferral, want 1 kept", n)
			}
			refresh(t, refresher, 1)
			expect(t, "2026-11-18", map[string]string{
				"//le1": "1|50|0", "register/R1/le1": "1|50|0", "item//le1": "1|50|0",
			})
		})
	}
}

// summaryOf reads one day's summary rows as "grain/grain_value/legal_entity" -> "row_count|total|line_amount".
func summaryOf(t *testing.T, conn *sql.DB, dialect db.Dialect, day string) map[string]string {
	t.Helper()
	q := dialect.QuoteIdent
	query := fmt.Sprintf("SELECT %s, %s, %s, %s, %s, %s FROM %s WHERE %s = ?",
		q("grain"), q("grain_value"), q("legal_entity"), q("row_count"), q("total"), q("line_amount"),
		q("_kora_fact_summary_sale"), q("business_date"))
	rows, err := conn.Query(db.Rebind(dialect, query), day)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var grain, value, entity, count, total, lineAmount string
		if err := rows.Scan(&grain, &value, &entity, &count, &total, &lineAmount); err != nil {
			t.Fatal(err)
		}
		out[grain+"/"+value+"/"+entity] = count + "|" + norm(t, total) + "|" + norm(t, lineAmount)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// assertMatchesFacts checks the day's total grain against a direct sum of the live header rows.
func assertMatchesFacts(t *testing.T, conn *sql.DB, dialect db.Dialect, day string, summary map[string]string) {
	t.Helper()
	q := dialect.QuoteIdent
	var n int
	var total string
	query := fmt.Sprintf("SELECT COUNT(*), COALESCE(SUM(%s), 0) FROM %s WHERE %s = ? AND %s = ? AND %s = ''",
		q("total"), q("_kora_fact_sale"), q("business_date"), q("deleted"), q("line_id"))
	if err := conn.QueryRow(db.Rebind(dialect, query), day, db.BoolParam(dialect, false)).Scan(&n, &total); err != nil {
		t.Fatalf("aggregate facts: %v", err)
	}
	have := summary["//le1"]
	if have == "" {
		have = "0|0|0"
	}
	parts := strings.Split(have, "|")
	if want := strconv.Itoa(n) + "|" + norm(t, total); parts[0]+"|"+parts[1] != want {
		t.Fatalf("summary %s total %s, facts say %s", day, parts[0]+"|"+parts[1], want)
	}
}

// countDirty returns how many dirty partition markers exist.
func countDirty(t *testing.T, conn *sql.DB, dialect db.Dialect) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", dialect.QuoteIdent(dirtyTable))).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// norm renders a SQL decimal in canonical form so MySQL and Postgres text agree.
func norm(t *testing.T, s string) string {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("decimal %q: %v", s, err)
	}
	return d.String()
}

// TestDirtyMarksAreOneRow marks the same day from ten transactions at once: no mark may fail,
// and exactly one marker must remain with version 10.
func TestDirtyMarksAreOneRow(t *testing.T) {
	for _, tc := range []struct{ driver, dialect, envVar string }{
		{"mysql", "mysql", "KORA_FACTS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_FACTS_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run", tc.envVar)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			dialect := db.Resolve(tc.dialect)
			reg := doctype.NewRegistry()
			reg.Register(saleFacts())
			b := &Builder{DB: database.DB, Dialect: dialect, Registry: reg}
			if err := b.ensure(context.Background(), saleFacts()); err != nil {
				t.Fatal(err)
			}
			day := tenanttime.Date{Year: 2026, Month: 11, Day: 18}
			var wg sync.WaitGroup
			errs := make(chan error, 10)
			for range 10 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					tx, err := database.DB.BeginTx(context.Background(), nil)
					if err != nil {
						errs <- err
						return
					}
					if err := b.markDirty(context.Background(), tx, "Sale", day); err != nil {
						_ = tx.Rollback()
						errs <- err
						return
					}
					errs <- tx.Commit()
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("concurrent mark: %v", err)
				}
			}
			var n int
			var version int64
			query := fmt.Sprintf("SELECT COUNT(*), MAX(%s) FROM %s", dialect.QuoteIdent("version"), dialect.QuoteIdent(dirtyTable))
			if err := database.DB.QueryRow(query).Scan(&n, &version); err != nil {
				t.Fatal(err)
			}
			if n != 1 || version != 10 {
				t.Fatalf("markers = %d, version = %d; want 1 and 10", n, version)
			}
		})
	}
}

// TestDirtyTableGainsColumns upgrades a dirty table created before the version and lease columns.
func TestDirtyTableGainsColumns(t *testing.T) {
	for _, tc := range []struct{ driver, dialect, envVar string }{
		{"mysql", "mysql", "KORA_FACTS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_FACTS_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run", tc.envVar)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			dialect := db.Resolve(tc.dialect)
			old := fmt.Sprintf("CREATE TABLE %s (model VARCHAR(140) NOT NULL, business_date DATE NOT NULL, PRIMARY KEY (model, business_date))", dialect.QuoteIdent(dirtyTable))
			if _, err := database.DB.Exec(old); err != nil {
				t.Fatal(err)
			}
			reg := doctype.NewRegistry()
			reg.Register(saleFacts())
			b := &Builder{DB: database.DB, Dialect: dialect, Registry: reg}
			if err := b.ensure(context.Background(), saleFacts()); err != nil {
				t.Fatalf("ensure over old table: %v", err)
			}
			tx, err := database.DB.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := b.markDirty(context.Background(), tx, "Sale", tenanttime.Date{Year: 2026, Month: 11, Day: 18}); err != nil {
				_ = tx.Rollback()
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			var version int64
			query := fmt.Sprintf("SELECT %s FROM %s", dialect.QuoteIdent("version"), dialect.QuoteIdent(dirtyTable))
			if err := database.DB.QueryRow(query).Scan(&version); err != nil || version != 1 {
				t.Fatalf("version = %d, %v; want 1", version, err)
			}
		})
	}
}
