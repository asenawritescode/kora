//go:build integration

package api

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/query"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

// D7 on real databases: Cashier with only the query grant reads own-module
// rows, never sees cost; another module's source still needs source read.
func TestOwnerQueryReadsWithoutSourceReadOnRealDatabases(t *testing.T) {
	cases := []struct{ name, driver, env, create string }{
		{"mysql", "mysql", "KORA_QUERY_TEST_MYSQL_DSN", "CREATE TABLE `tabItem` (name VARCHAR(40) PRIMARY KEY, item_name VARCHAR(40), price DECIMAL(10,2), cost DECIMAL(10,2))"},
		{"postgres", "postgres", "KORA_QUERY_TEST_POSTGRES_DSN", `CREATE TABLE "tabItem" (name VARCHAR(40) PRIMARY KEY, item_name VARCHAR(40), price NUMERIC(10,2), cost NUMERIC(10,2))`},
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
			tbl := d.QuoteIdent("tabItem")
			_, _ = conn.Exec("DROP TABLE " + tbl)
			if _, err := conn.Exec(tc.create); err != nil {
				t.Fatal(err)
			}
			defer conn.Exec("DROP TABLE " + tbl)
			if _, err := conn.Exec(db.Rebind(d, "INSERT INTO "+tbl+" VALUES (?,?,?,?)"), "I1", "Soap", "5.00", "2.00"); err != nil {
				t.Fatal(err)
			}
			scope := contract.BusinessScope{TenantID: "t", Kind: contract.ScopeKindTenant}
			pq := contract.PublishedQuery{OwnerModuleID: "inventory.core", Name: "item_search", ScopePolicy: contract.ScopeTenantShared, AllowedFields: []string{"name", "item_name", "price"}}
			exec, err := policy.NewVerifiedExecution("t", contract.ActorContext{PrincipalID: "c", PrincipalType: contract.PrincipalHuman, Site: "r", Roles: []string{"Cashier"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, owner := range []string{"inventory.core", "pos.core"} {
				reg := doctype.NewRegistry()
				reg.Register(&doctype.DocType{Name: "Item", ModuleOwnerID: owner})
				reg.Permissions.SetPermission(&doctype.Permission{Doctype: "module.inventory.core.query.item_search", Role: "Cashier", Read: true})
				reg.SetQuerySource("module.inventory.core.query.item_search", "Item")
				authErr := authorizePublishedQuery(exec, reg, pq, scope)
				if owner == "pos.core" {
					if authErr == nil {
						t.Fatal("other-module source allowed without source read")
					}
					continue
				}
				if authErr != nil {
					t.Fatalf("own-module denied: %v", authErr)
				}
			}
			def := &query.Definition{Name: "item_search", Source: "Item", Fields: pq.AllowedFields, Limit: 5}
			p, err := query.Run(context.Background(), conn, d, def, query.Request{FullRead: true})
			if err != nil || len(p.Rows) != 1 {
				t.Fatalf("run: %+v %v", p, err)
			}
			if _, has := p.Rows[0]["cost"]; has {
				t.Fatalf("cost leaked: %v", p.Rows[0])
			}
		})
	}
}
