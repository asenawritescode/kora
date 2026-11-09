//go:build integration

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/query"
)

// Set KORA_PRINT_QUERY_MYSQL_DSN / KORA_PRINT_QUERY_POSTGRES_DSN to disposable servers.
func TestPrintQueriesOnRealDatabases(t *testing.T) {
	for _, tc := range []struct{ driver, env, num string }{
		{"mysql", "KORA_PRINT_QUERY_MYSQL_DSN", "DECIMAL(10,2)"},
		{"postgres", "KORA_PRINT_QUERY_POSTGRES_DSN", "NUMERIC(10,2)"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			conn, err := sql.Open(tc.driver, dsn)
			if err != nil || conn.Ping() != nil {
				t.Fatalf("connect: %v", err)
			}
			defer conn.Close()
			d := kdb.Resolve(tc.driver)
			sale, pay := d.QuoteIdent("tabSale"), d.QuoteIdent("tabSale__payments")
			_, _ = conn.Exec("DROP TABLE " + pay)
			_, _ = conn.Exec("DROP TABLE " + sale)
			defer func() { _, _ = conn.Exec("DROP TABLE " + pay); _, _ = conn.Exec("DROP TABLE " + sale) }()
			for _, q := range []string{
				"CREATE TABLE " + sale + " (name VARCHAR(40) PRIMARY KEY, status VARCHAR(20), till_session VARCHAR(40))",
				"CREATE TABLE " + pay + " (name VARCHAR(40) PRIMARY KEY, parent VARCHAR(40), method VARCHAR(20), amount " + tc.num + ")",
				"INSERT INTO " + sale + " VALUES ('S1','Paid','T-1'),('S2','Paid','T-1'),('S3','Paid','T-2')",
				"INSERT INTO " + pay + " VALUES ('P1','S1','Cash',10.10),('P2','S1','Card',5.20),('P3','S2','Cash',2.30),('P4','S3','Cash',99.00)",
			} {
				if _, err := conn.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			reg := doctype.NewRegistry()
			reg.Register(&doctype.DocType{Name: "Sale", Fields: []doctype.Field{{Fieldname: "status", Fieldtype: "Select"}, {Fieldname: "till_session", Fieldtype: "Data"}, {Fieldname: "payments", Fieldtype: "Table", Options: "Sale Payment"}}})
			reg.Register(&doctype.DocType{Name: "Sale Payment", IsChildTable: true, Fields: []doctype.Field{{Fieldname: "method", Fieldtype: "Data"}, {Fieldname: "amount", Fieldtype: "Currency"}}})
			def, err := query.Parse([]byte(`
name: today_totals
module_owner_id: pos.core
capability: pos.sales
operation: totals
source: Sale Payment
via: Sale.payments
permission: read
scope: tenant
input:
  till_session: {type: string, max_length: 140}
filters:
  - {field: parent.status, op: eq, value: Paid}
  - {field: parent.till_session, op: eq, value: $input.till_session}
group_by: [method]
aggregates: [{name: total, fn: sum, field: amount}]
sort: [{field: method}]
`), reg)
			if err != nil {
				t.Fatal(err)
			}
			exec := queryFn(func(ctx context.Context, in json.RawMessage) ([]map[string]any, error) {
				var input map[string]any
				_ = json.Unmarshal(in, &input)
				pg, err := query.Run(ctx, conn, d, def, query.Request{FullRead: true, Input: input})
				if err != nil {
					return nil, err
				}
				return pg.Rows, nil
			})
			w := printQueryRequest(t, exec, "Manager")
			if w.Code != 200 || !strings.Contains(w.Body.String(), "Card=5.2") || !strings.Contains(w.Body.String(), "Cash=12.4") || strings.Contains(w.Body.String(), "99") {
				t.Fatalf("manager: %d %q", w.Code, w.Body.String())
			}
			if w := printQueryRequest(t, exec, "Cashier"); w.Code != 403 || strings.Contains(w.Body.String(), "Cash=") {
				t.Fatalf("cashier: %d %q", w.Code, w.Body.String())
			}
		})
	}
}
