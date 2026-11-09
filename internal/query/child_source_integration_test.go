//go:build integration

package query

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

// Sale Payment rows live in "tabSale__payments"; sums run on child rows, filtered by the parent.
func TestChildSourceOnRealDatabases(t *testing.T) {
	cases := []struct{ name, env, num string }{
		{"mysql", "KORA_QUERY_TEST_MYSQL_DSN", "DECIMAL(10,2)"},
		{"postgres", "KORA_QUERY_TEST_POSTGRES_DSN", "NUMERIC(10,2)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skip("set " + tc.env)
			}
			conn, err := sql.Open(tc.name, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			d := db.Resolve(tc.name)
			sale, pay := d.QuoteIdent("tabSale"), d.QuoteIdent("tabSale__payments")
			for _, tbl := range []string{pay, sale} {
				_, _ = conn.Exec("DROP TABLE " + tbl)
			}
			defer func() { _, _ = conn.Exec("DROP TABLE " + pay); _, _ = conn.Exec("DROP TABLE " + sale) }()
			for _, ddl := range []string{
				"CREATE TABLE " + sale + " (name VARCHAR(40) PRIMARY KEY, status VARCHAR(20))",
				"CREATE TABLE " + pay + " (name VARCHAR(40) PRIMARY KEY, parent VARCHAR(40), method VARCHAR(20), amount " + tc.num + ")",
			} {
				if _, err := conn.Exec(ddl); err != nil {
					t.Fatal(err)
				}
			}
			for _, s := range [][2]string{{"S1", "Paid"}, {"S2", "Paid"}, {"S3", "Void"}} {
				if _, err := conn.Exec(db.Rebind(d, "INSERT INTO "+sale+" VALUES (?,?)"), s[0], s[1]); err != nil {
					t.Fatal(err)
				}
			}
			for i, p := range [][3]string{{"S1", "Cash", "10.10"}, {"S1", "Card", "5.20"}, {"S2", "Cash", "2.30"}, {"S3", "Cash", "99.00"}} {
				if _, err := conn.Exec(db.Rebind(d, "INSERT INTO "+pay+" VALUES (?,?,?,?)"), fmt.Sprintf("P%d", i+1), p[0], p[1], p[2]); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			agg, err := Parse([]byte(childAgg), childReg())
			if err != nil {
				t.Fatal(err)
			}
			pg, err := Run(ctx, conn, d, agg, Request{FullRead: true})
			if err != nil || len(pg.Rows) != 2 {
				t.Fatalf("agg: %+v %v", pg, err)
			}
			want := map[string][2]string{"Card": {"1", "5.2"}, "Cash": {"2", "12.4"}}
			for _, r := range pg.Rows {
				w := want[fmt.Sprint(r["method"])]
				if fmt.Sprint(r["n"]) != w[0] || fmt.Sprintf("%v", mustFloat(r["total"])) != w[1] {
					t.Fatalf("row %v want %v", r, w)
				}
			}
			list, err := Parse([]byte(`
name: l
module_owner_id: m
capability: c
operation: o
source: Sale Payment
via: Sale.payments
permission: read
scope: tenant
limit: 2
filters: [{field: parent.status, op: eq, value: Paid}]
fields: [name, method]
sort: [{field: method}]
`), childReg())
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			var cur []any
			for i := 0; i < 5; i++ {
				pg, err := Run(ctx, conn, d, list, Request{FullRead: true, Cursor: cur})
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range pg.Rows {
					got = append(got, fmt.Sprint(r["name"]))
				}
				if pg.Next == nil {
					break
				}
				cur = pg.Next
			}
			if fmt.Sprint(got) != "[P2 P1 P3]" {
				t.Fatalf("paged = %v", got)
			}
		})
	}
}

func mustFloat(v any) float64 {
	f, _ := strconv.ParseFloat(fmt.Sprint(v), 64)
	return f
}
