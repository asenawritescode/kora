//go:build integration

package ai

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/outbox"
	ksite "github.com/asenawritescode/kora/internal/site"
)

// TestFindRecordsMatchesNamesAcrossDialects runs find_records on the parked purchasing pack. Link filters and the
// search match supplier and bill names, and a miss says how many bills exist.
func TestFindRecordsMatchesNamesAcrossDialects(t *testing.T) {
	for _, tc := range []struct {
		name string
		dsn  string
	}{
		{name: "mysql", dsn: "KORA_AI_LINK_SEARCH_MYSQL_DSN"},
		{name: "postgres", dsn: "KORA_AI_LINK_SEARCH_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.dsn)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s database", tc.dsn, tc.name)
			}
			database, err := sql.Open(tc.name, dsn)
			if err != nil {
				t.Fatalf("open %s database: %v", tc.name, err)
			}
			t.Cleanup(func() { _ = database.Close() })
			if err = database.Ping(); err != nil {
				t.Fatalf("ping %s database: %v", tc.name, err)
			}
			dialect := kdb.Resolve(tc.name)
			ctx := WithDialect(context.Background(), dialect)
			if err = ksite.BootstrapSystemTables(ctx, database, dialect); err != nil {
				t.Fatalf("bootstrap %s system tables: %v", tc.name, err)
			}
			registry := loadPackRegistry(t, "_parked/erp-purchasing")
			for _, dt := range registry.All() {
				for _, statement := range dialect.CreateTable(dt) {
					if _, err = database.Exec(statement); err != nil {
						t.Fatalf("create %s table %s: %v", tc.name, dt.Name, err)
					}
				}
			}
			site := fmt.Sprintf("ai-link-search-%s-%x", tc.name, time.Now().UnixNano())
			execution := testAIExecution(t, site, "alice@example.test", []string{doctype.AdminRole})
			tx := &orm.TxManager{
				DB: database, Registry: registry, Dialect: dialect, SiteName: site,
				Context: ctx, CurrentUser: "alice@example.test", CurrentUserRole: doctype.AdminRole,
				CurrentUserRoles: []string{doctype.AdminRole}, Outbox: outbox.NewSQLWriter(dialect), Execution: execution,
			}
			seedPurchasing(t, tx, database, registry)

			turn := 0
			find := func(args string) string {
				turn++
				call := map[string]any{"id": fmt.Sprintf("call-%d", turn), "type": "function", "function": map[string]any{"name": toolFindRecords, "arguments": args}}
				results := RunToolTurn(tx.Context, tx, registry, toolCallBatch{
					Calls: []any{call}, Owner: "alice@example.test", Site: site, RunID: fmt.Sprintf("run-%d", turn),
					StepID: "step", ConversationID: "conversation-" + site, Execution: execution, Commands: kernel.NewCommandRegistry(),
				})
				content, _ := results[0]["content"].(string)
				return content
			}

			for _, tc := range []struct {
				name string
				args string
				want []string
			}{
				{"like matches the supplier name", `{"record_type":"Supplier Bill","filters":[{"field":"supplier","op":"like","value":"Ouma Son"}]}`,
					[]string{`Matched supplier "Ouma Son" → Mr. Ouma Son (SUPP-0001).`, "Found 2 Supplier Bill, showing 2:"}},
				{"typo in the name is matched, case-insensitively", `{"record_type":"Supplier Bill","filters":[{"field":"supplier","op":"like","value":"OUMAS SON"}]}`,
					[]string{`Matched supplier "OUMAS SON" → Mr. Ouma Son (SUPP-0001).`, "Found 2 Supplier Bill, showing 2:"}},
				{"one word matches three suppliers", `{"record_type":"Supplier Bill","filters":[{"field":"supplier","op":"like","value":"ouma"}]}`,
					[]string{"Matched supplier \"ouma\" → 3 suppliers: Mr. Ouma Granary (SUPP-0002), Mr. Ouma Son (SUPP-0001), Patrice Ouma (SUPP-0003).", "Found 2 Supplier Bill, showing 2:"}},
				{"a supplier with no bills gives the honest zero", `{"record_type":"Supplier Bill","filters":[{"field":"supplier","op":"like","value":"Granary"}]}`,
					[]string{`Matched supplier "Granary" → Mr. Ouma Granary (SUPP-0002).`, "No Supplier Bill match these filters. 2 Supplier Bill exist in total."}},
				{"a name with no match lists the closest suppliers", `{"record_type":"Supplier Bill","filters":[{"field":"supplier","op":"=","value":"zzz"}]}`,
					[]string{`No Supplier matches "zzz". Closest:`}},
				{"search matches a bill code", `{"record_type":"Supplier Bill","search":"SB-0002"}`,
					[]string{"Found 1 Supplier Bill, showing 1:"}},
				{"search with no match gives the total", `{"record_type":"Supplier Bill","search":"zzz"}`,
					[]string{"No Supplier Bill match these filters. 2 Supplier Bill exist in total."}},
				{"no filters lists every bill", `{"record_type":"Supplier Bill"}`,
					[]string{"Found 2 Supplier Bill, showing 2:"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got := find(tc.args)
					for _, want := range tc.want {
						if !strings.Contains(got, want) {
							t.Fatalf("find_records on %s = %q, want it to contain %q", tc.name, got, want)
						}
					}
				})
			}
		})
	}
}

// seedPurchasing writes three suppliers and two bills of the first supplier, as the purchasing pack stores them.
func seedPurchasing(t *testing.T, tx *orm.TxManager, database *sql.DB, registry *doctype.Registry) {
	t.Helper()
	dbTx, err := database.Begin()
	if err != nil {
		t.Fatal("begin seed:", err)
	}
	insert := func(dtName, name string, fields map[string]any) {
		doc := doctype.NewDocument(dtName)
		doc.Name = name
		doc.Fields = fields
		if err := tx.InsertInTx(dbTx, registry.Get(dtName), doc, "alice@example.test", "alice@example.test"); err != nil {
			_ = dbTx.Rollback()
			t.Fatalf("insert %s %s: %v", dtName, name, err)
		}
	}
	insert("Supplier", "SUPP-0001", map[string]any{"supplier_name": "Mr. Ouma Son"})
	insert("Supplier", "SUPP-0002", map[string]any{"supplier_name": "Mr. Ouma Granary"})
	insert("Supplier", "SUPP-0003", map[string]any{"supplier_name": "Patrice Ouma"})
	insert("Supplier Bill", "SB-0001", map[string]any{"reference": "SB-0001", "supplier": "SUPP-0001", "bill_date": "2026-11-01", "amount": 1000})
	insert("Supplier Bill", "SB-0002", map[string]any{"reference": "SB-0002", "supplier": "SUPP-0001", "bill_date": "2026-11-02", "amount": 500})
	if err := dbTx.Commit(); err != nil {
		t.Fatal("commit seed:", err)
	}
}
