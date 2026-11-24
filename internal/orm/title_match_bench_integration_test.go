//go:build integration

package orm

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
	_ "github.com/lib/pq"
)

// BenchmarkMatchTitles times the title match and the capped count on the real Supplier type of the parked
// purchasing pack at 500, 10,000 and 100,000 rows. Run it locally, not in CI:
//
//	KORA_TITLE_MATCH_BENCH_MYSQL_DSN=... go test -tags integration -run '^$' -bench MatchTitles ./internal/orm/
func BenchmarkMatchTitles(b *testing.B) {
	for _, driver := range []struct{ name, env string }{
		{"mysql", "KORA_TITLE_MATCH_BENCH_MYSQL_DSN"},
		{"postgres", "KORA_TITLE_MATCH_BENCH_POSTGRES_DSN"},
	} {
		dsn := strings.TrimSpace(os.Getenv(driver.env))
		if dsn == "" {
			b.Skipf("set %s to a disposable %s database", driver.env, driver.name)
		}
		benchDriver(b, driver.name, dsn)
	}
}

// benchDriver runs the benchmark sizes on one disposable database.
func benchDriver(b *testing.B, driverName, dsn string) {
	{
		database, err := sql.Open(driverName, dsn)
		if err != nil {
			b.Fatal("open database:", err)
		}
		defer database.Close()
		dialect := db.Resolve(driverName)
		doctypes, err := doctype.ParseConfigTree("../../config/_parked/erp-purchasing")
		if err != nil {
			b.Fatal("parse purchasing pack:", err)
		}
		registry := doctype.NewRegistry()
		for _, dt := range doctypes {
			registry.Register(dt)
		}
		dt := registry.Get("Supplier")
		tx := &TxManager{DB: database, Registry: registry, Dialect: dialect}
		table := tx.tableName(dt)
		_, _ = database.Exec("DROP TABLE IF EXISTS " + table)
		for _, statement := range dialect.CreateTable(dt) {
			if _, err := database.Exec(statement); err != nil {
				b.Fatal("create Supplier table:", err)
			}
		}
		for _, size := range []int{500, 10_000, 100_000} {
			seedSuppliers(b, database, dialect, table, size)
			ctx := context.Background()
			b.Run(fmt.Sprintf("%s/words/%d", driverName, size), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					if _, err := tx.MatchTitles(ctx, dt, "ouma son", policy.RowScope{}); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(fmt.Sprintf("%s/typo/%d", driverName, size), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					if _, err := tx.MatchTitles(ctx, dt, "oumas sun", policy.RowScope{}); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(fmt.Sprintf("%s/count/%d", driverName, size), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					if _, err := tx.CountUpTo(ctx, dt, policy.RowScope{}, 1001); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// seedSuppliers replaces the rows of table with size suppliers, one in a thousand named like the word-match query.
func seedSuppliers(b *testing.B, database *sql.DB, dialect db.Dialect, table string, size int) {
	b.Helper()
	if _, err := database.Exec("DELETE FROM " + table); err != nil {
		b.Fatal("clear Supplier rows:", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	const batch = 500
	for start := 0; start < size; start += batch {
		var placeholders []string
		var args []any
		for i := start; i < start+batch && i < size; i++ {
			title := fmt.Sprintf("Supplier %06d", i)
			if i%1000 == 0 {
				title = fmt.Sprintf("Mr. Ouma Son %d", i)
			}
			placeholders = append(placeholders, "(?,?,?,?,?,?,?,?)")
			args = append(args, fmt.Sprintf("SUPP-%06d", i), title, "bench@example.test", now, now, "bench@example.test", 0, 0)
		}
		statement := "INSERT INTO " + table + " (name, supplier_name, owner, creation, modified, modified_by, doc_status, idx) VALUES " + strings.Join(placeholders, ",")
		if _, err := database.Exec(db.Rebind(dialect, statement), args...); err != nil {
			b.Fatal("seed Supplier rows:", err)
		}
	}
}
