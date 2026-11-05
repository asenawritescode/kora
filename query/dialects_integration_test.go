//go:build integration

package query

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/asenawritescode/kora/db"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

// Run-time paging, aggregates, optional filters and owner/scope fencing on real MySQL and PostgreSQL.
func TestRunPagingAggregatesAndScopeOnRealDatabases(t *testing.T) {
	cases := []struct{ name, driver, env, create string }{
		{"mysql", "mysql", "KORA_QUERY_TEST_MYSQL_DSN", "CREATE TABLE `tabOrder` (name VARCHAR(40) PRIMARY KEY, owner VARCHAR(40), region VARCHAR(20), status VARCHAR(20), total DECIMAL(10,2))"},
		{"postgres", "postgres", "KORA_QUERY_TEST_POSTGRES_DSN", `CREATE TABLE "tabOrder" (name VARCHAR(40) PRIMARY KEY, owner VARCHAR(40), region VARCHAR(20), status VARCHAR(20), total NUMERIC(10,2))`},
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
			d := db.Resolve(tc.name)
			tbl := d.QuoteIdent("tabOrder")
			_, _ = conn.Exec("DROP TABLE " + tbl)
			if _, err := conn.Exec(tc.create); err != nil {
				t.Fatal(err)
			}
			defer conn.Exec("DROP TABLE " + tbl)
			ins := db.Rebind(d, "INSERT INTO "+tbl+" VALUES (?,?,?,?,?)")
			for _, r := range [][]any{
				{"O1", "al", "east", "Paid", "10.50"}, {"O2", "bob", "east", "Paid", "4.50"},
				{"O3", "al", "west", "Void", "6.00"}, {"O4", "al", "west", "Paid", "1.00"}, {"O5", "bob", "west", "Paid", "2.00"},
			} {
				if _, err := conn.Exec(ins, r...); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()

			// Cursor paging walks every row once, in order, with a next cursor until the last page.
			list := &Definition{Name: "all", Source: "Order", Fields: []string{"name"}, Limit: 2, Sort: []Sort{{Field: "name"}}}
			var names []string
			req := Request{FullRead: true}
			for pages := 0; pages < 5; pages++ {
				p, err := Run(ctx, conn, d, list, req)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range p.Rows {
					names = append(names, r["name"].(string))
				}
				if p.Next == nil {
					break
				}
				req.Cursor = p.Next
			}
			if got := len(names); got != 5 || names[0] != "O1" || names[4] != "O5" {
				t.Fatalf("paged names = %v", names)
			}

			// Owner fence: a non-full-read actor sees only own rows.
			own := *list
			own.OwnerField = "owner"
			own.Limit = 10
			p, err := Run(ctx, conn, d, &own, Request{Actor: "bob"})
			if err != nil || len(p.Rows) != 2 {
				t.Fatalf("owner fence: %+v %v", p, err)
			}

			// Grouped aggregates with an optional filter, applied and skipped.
			agg := &Definition{Name: "totals", Source: "Order", Limit: 10, Fields: []string{"region", "n", "sum_total"}, GroupBy: []string{"region"},
				Aggregates: []Aggregate{{Fn: "count", Name: "n"}, {Fn: "sum", Field: "total", Name: "sum_total"}},
				Input:      map[string]InputSpec{"st": {Type: "string"}},
				Filters:    []Filter{{Field: "status", Op: "eq", Value: "$input.st", Optional: true}},
			}
			p, err = Run(ctx, conn, d, agg, Request{FullRead: true})
			if err != nil || len(p.Rows) != 2 {
				t.Fatalf("aggregate all: %+v %v", p, err)
			}
			counts := map[string]string{}
			for _, r := range p.Rows {
				counts[r["region"].(string)] = fmtAny(r["n"])
			}
			if counts["east"] != "2" || counts["west"] != "3" {
				t.Fatalf("counts = %v", counts)
			}
			p, err = Run(ctx, conn, d, agg, Request{FullRead: true, Input: map[string]any{"st": "Void"}})
			if err != nil || len(p.Rows) != 1 || p.Rows[0]["region"] != "west" || fmtAny(p.Rows[0]["n"]) != "1" {
				t.Fatalf("aggregate filtered: %+v %v", p, err)
			}
		})
	}
}

func fmtAny(v any) string {
	switch x := v.(type) {
	case int64:
		return string(rune('0' + x))
	case []byte:
		return string(x)
	case string:
		return x
	}
	return "?"
}
