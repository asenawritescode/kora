//go:build integration

package query

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

// Real tenants store DocType "Sale" in table "tabSale"; Run must read it.
func TestRunReadsPrefixedTableOnRealDatabases(t *testing.T) {
	cases := []struct{ name, driver, env, dialect, create string }{
		{"mysql", "mysql", "KORA_QUERY_TEST_MYSQL_DSN", "mysql", "CREATE TABLE `tabSale` (name VARCHAR(40) PRIMARY KEY, status VARCHAR(20), total DECIMAL(10,2))"},
		{"postgres", "postgres", "KORA_QUERY_TEST_POSTGRES_DSN", "postgres", `CREATE TABLE "tabSale" (name VARCHAR(40) PRIMARY KEY, status VARCHAR(20), total NUMERIC(10,2))`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skip("set " + tc.env)
			}
			conn, err := sql.Open(tc.driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			d := db.Resolve(tc.dialect)
			tbl := d.QuoteIdent("tabSale")
			_, _ = conn.Exec("DROP TABLE " + tbl)
			if _, err := conn.Exec(tc.create); err != nil {
				t.Fatal(err)
			}
			defer conn.Exec("DROP TABLE " + tbl)
			if _, err := conn.Exec(db.Rebind(d, "INSERT INTO "+tbl+" VALUES (?,?,?), (?,?,?)"), "S1", "Paid", "10.50", "S2", "Paid", "4.50"); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			list := &Definition{Name: "recent", Source: "Sale", Fields: []string{"name", "status"}, Limit: 5, Sort: []Sort{{Field: "name", Dir: "asc"}}}
			p, err := Run(ctx, conn, d, list, Request{FullRead: true})
			if err != nil || len(p.Rows) != 2 {
				t.Fatalf("list: %+v %v", p, err)
			}
			agg := &Definition{Name: "totals", Source: "Sale", Limit: 10, Fields: []string{"status", "n", "sum_total"}, GroupBy: []string{"status"}, Aggregates: []Aggregate{{Fn: "count", Name: "n"}, {Fn: "sum", Field: "total", Name: "sum_total"}}}
			p, err = Run(ctx, conn, d, agg, Request{FullRead: true})
			if err != nil || len(p.Rows) != 1 {
				t.Fatalf("aggregate: %+v %v", p, err)
			}
		})
	}
}
