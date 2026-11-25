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
	"github.com/asenawritescode/kora/internal/policy"
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
			run := func(execution policy.Execution, tool, args string) string {
				turn++
				call := map[string]any{"id": fmt.Sprintf("call-%d", turn), "type": "function", "function": map[string]any{"name": tool, "arguments": args}}
				results := RunToolTurn(tx.Context, tx, registry, toolCallBatch{
					Calls: []any{call}, Owner: "alice@example.test", Site: site, RunID: fmt.Sprintf("run-%d", turn),
					StepID: "step", ConversationID: "conversation-" + site, Execution: execution, Commands: kernel.NewCommandRegistry(),
				})
				content, _ := results[0]["content"].(string)
				return content
			}
			find := func(args string) string { return run(execution, toolFindRecords, args) }

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
				{"no filters lists every bill with the supplier's name", `{"record_type":"Supplier Bill"}`,
					[]string{"Found 2 Supplier Bill, showing 2:", "Supplier: Mr. Ouma Son (SUPP-0001)"}},
				{"search matches the supplier name when the bill title does not", `{"record_type":"Supplier Bill","search":"oumas son"}`,
					[]string{`Matched supplier "oumas son" → Mr. Ouma Son (SUPP-0001).`, "Found 2 Supplier Bill, showing 2:"}},
				{"search of a shared word matches several suppliers", `{"record_type":"Supplier Bill","search":"Ouma"}`,
					[]string{"→ 3 suppliers:", "Found 2 Supplier Bill, showing 2:"}},
				{"search of a supplier with no bills gives the honest zero", `{"record_type":"Supplier Bill","search":"Granary"}`,
					[]string{`Matched supplier "Granary" → Mr. Ouma Granary (SUPP-0002).`, "No Supplier Bill match these filters. 2 Supplier Bill exist in total."}},
				{"search matches a supplier code without a name note", `{"record_type":"Supplier Bill","search":"SUPP-0001"}`,
					[]string{"Found 2 Supplier Bill, showing 2:", "!Matched"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got := find(tc.args)
					for _, want := range tc.want {
						// A "!" prefix asserts the text is absent.
						if text, absent := strings.CutPrefix(want, "!"); absent {
							if strings.Contains(got, text) {
								t.Fatalf("find_records on %s = %q, want it not to contain %q", tc.name, got, text)
							}
							continue
						}
						if !strings.Contains(got, want) {
							t.Fatalf("find_records on %s = %q, want it to contain %q", tc.name, got, want)
						}
					}
				})
			}

			t.Run("get_record gives the Link name beside the code", func(t *testing.T) {
				got := run(execution, toolGetRecord, `{"record_type":"Supplier Bill","name":"SB-0001"}`)
				for _, want := range []string{`"link_titles":{"supplier":"Mr. Ouma Son"}`, `"supplier":"SUPP-0001"`} {
					if !strings.Contains(got, want) {
						t.Fatalf("get_record = %q, want it to contain %q", got, want)
					}
				}
			})

			t.Run("a reader without Supplier read sees the codes alone", func(t *testing.T) {
				registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Supplier Bill", Role: "Bill Clerk", Read: true})
				clerk := testAIExecution(t, site, "clerk@example.test", []string{"Bill Clerk"})
				found := run(clerk, toolFindRecords, `{"record_type":"Supplier Bill"}`)
				if !strings.Contains(found, "Supplier: SUPP-0001") || strings.Contains(found, "Mr. Ouma Son") {
					t.Fatalf("find_records as clerk = %q, want the code alone", found)
				}
				got := run(clerk, toolGetRecord, `{"record_type":"Supplier Bill","name":"SB-0001"}`)
				if strings.Contains(got, "link_titles") || strings.Contains(got, "Mr. Ouma Son") || !strings.Contains(got, `"supplier":"SUPP-0001"`) {
					t.Fatalf("get_record as clerk = %q, want the code and no link_titles", got)
				}
			})

			t.Run("a reader without Supplier read searches bills by the honest zero, not the supplier name", func(t *testing.T) {
				registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Supplier Bill", Role: "Bill Clerk", Read: true})
				clerk := testAIExecution(t, site, "clerk@example.test", []string{"Bill Clerk"})
				got := run(clerk, toolFindRecords, `{"record_type":"Supplier Bill","search":"oumas son"}`)
				if !strings.Contains(got, "No Supplier Bill match these filters. 2 Supplier Bill exist in total.") {
					t.Fatalf("find_records search as clerk = %q, want the honest zero", got)
				}
				if strings.Contains(got, "Mr. Ouma Son") || strings.Contains(got, "Matched") {
					t.Fatalf("find_records search as clerk = %q, want no supplier name", got)
				}
			})

			// The bill below has the title BILL-001 and the code SB-0003, so only the title names it. It is written after
			// the subtests above, which count the bills.
			seedBill(t, tx, database, registry, "SB-0003", map[string]any{"reference": "BILL-001", "supplier": "SUPP-0001", "bill_date": "2026-11-03", "amount": 700})

			t.Run("get_record opens a bill by its title, with a note and the Link name", func(t *testing.T) {
				got := run(execution, toolGetRecord, `{"record_type":"Supplier Bill","name":"BILL-001"}`)
				for _, want := range []string{`"name":"SB-0003"`, `"note":"BILL-001 is SB-0003."`, `"link_titles":{"supplier":"Mr. Ouma Son"}`} {
					if !strings.Contains(got, want) {
						t.Fatalf("get_record by title = %q, want it to contain %q", got, want)
					}
				}
			})

			t.Run("get_record opens a bill by its title in lower case", func(t *testing.T) {
				got := run(execution, toolGetRecord, `{"record_type":"Supplier Bill","name":"bill-001"}`)
				for _, want := range []string{`"name":"SB-0003"`, `"note":"bill-001 is SB-0003."`} {
					if !strings.Contains(got, want) {
						t.Fatalf("get_record by lower-case title = %q, want it to contain %q", got, want)
					}
				}
			})

			t.Run("get_record opens a code typed in lower case on both dialects", func(t *testing.T) {
				got := run(execution, toolGetRecord, `{"record_type":"Supplier Bill","name":"sb-0001"}`)
				if !strings.Contains(got, `"name":"SB-0001"`) {
					t.Fatalf("get_record of sb-0001 = %q, want the record SB-0001", got)
				}
			})

			t.Run("get_record of a supplier code typed in lower case suggests the code", func(t *testing.T) {
				got := run(execution, toolGetRecord, `{"record_type":"Supplier","name":"supp-001"}`)
				if !strings.Contains(got, "was not found. Did you mean SUPP-0001") {
					t.Fatalf("get_record of supp-001 = %q, want a suggestion of SUPP-0001", got)
				}
			})

			t.Run("get_record of a name with no near code is a plain not found", func(t *testing.T) {
				got := run(execution, toolGetRecord, `{"record_type":"Supplier","name":"zzz"}`)
				if !strings.Contains(got, "was not found.") || strings.Contains(got, "Did you mean") {
					t.Fatalf("get_record of zzz = %q, want a plain not found", got)
				}
			})

			t.Run("a reader who owns no bill cannot open another bill by its title", func(t *testing.T) {
				registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Supplier Bill", Role: "Own Bill Clerk", Read: true, IfOwner: true})
				clerk := testAIExecution(t, site, "own-clerk@example.test", []string{"Own Bill Clerk"})
				got := run(clerk, toolGetRecord, `{"record_type":"Supplier Bill","name":"BILL-001"}`)
				if !strings.Contains(got, "was not found.") || strings.Contains(got, "SB-0003") || strings.Contains(got, "note") {
					t.Fatalf("get_record by title as an owner-only reader = %q, want not found and no leak", got)
				}
			})
		})
	}
}

// seedBill writes one Supplier Bill under its code, in its own transaction.
func seedBill(t *testing.T, tx *orm.TxManager, database *sql.DB, registry *doctype.Registry, name string, fields map[string]any) {
	t.Helper()
	dbTx, err := database.Begin()
	if err != nil {
		t.Fatal("begin bill seed:", err)
	}
	doc := doctype.NewDocument("Supplier Bill")
	doc.Name = name
	doc.Fields = fields
	if err := tx.InsertInTx(dbTx, registry.Get("Supplier Bill"), doc, "alice@example.test", "alice@example.test"); err != nil {
		_ = dbTx.Rollback()
		t.Fatalf("insert Supplier Bill %s: %v", name, err)
	}
	if err := dbTx.Commit(); err != nil {
		t.Fatal("commit bill seed:", err)
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
