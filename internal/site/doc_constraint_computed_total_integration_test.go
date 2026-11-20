//go:build integration

package site

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
)

// Journal-style doctype: the totals are computed from the lines and the
// balance rule compares them. The client never sends the totals.
const journalEntryYAML = `name: Journal Entry
module: Journal Test
title_field: label
fields:
  - fieldname: label
    fieldtype: Data
    label: Label
  - fieldname: total_debit
    fieldtype: Currency
    label: Total Debit
    computed: "SUM(lines.debit)"
  - fieldname: total_credit
    fieldtype: Currency
    label: Total Credit
    computed: "SUM(lines.credit)"
  - fieldname: lines
    fieldtype: Table
    options: Journal Line
    label: Lines
doc_constraints:
  - type: cross_field
    lhs: total_debit
    operator: "=="
    rhs: total_credit
    message: Debits and credits must be equal.
`

const journalLineYAML = `name: Journal Line
module: Journal Test
is_child_table: true
fields:
  - fieldname: debit
    fieldtype: Currency
    label: Debit
  - fieldname: credit
    fieldtype: Currency
    label: Credit
`

const postJournalYAML = `name: post_journal
namespace: journal
version: 1
permission: create
input:
  record: Journal Entry
transaction:
  - as: entry
    create:
      record: Journal Entry
      values:
        label: $input.label
        lines: $input.lines
output:
  reference: $steps.entry.name
`

// TestDocConstraintComputedTotalsAcrossDialects checks that a balance rule over
// computed child totals sees the totals the save will store. An unbalanced
// journal is refused and a balanced one accepted, on MySQL and Postgres.
func TestDocConstraintComputedTotalsAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_DOCTYPE_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_DOCTYPE_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			conn, name := posDB(t, tc.driver, dsn)
			runDocConstraintComputedTotals(t, tc.driver, conn, name)
		})
	}
}

func runDocConstraintComputedTotals(t *testing.T, driver string, conn *sql.DB, dbName string) {
	ctx := context.Background()
	dialect := kdb.Resolve(driver)
	if err := BootstrapSystemTables(ctx, conn, dialect); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for path, body := range map[string]string{
		"doctypes/journal_entry.yaml": journalEntryYAML,
		"doctypes/journal_line.yaml":  journalLineYAML,
		"commands/post_journal.yaml":  postJournalYAML,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg := doctype.NewRegistry()
	const siteName = "doc-constraint-computed-total.example.test"
	if err := ImportConfig(conn, reg, dbName, siteName, dir, dialect); err != nil {
		t.Fatal(err)
	}
	cmds := kernel.NewCommandRegistry()
	defs, err := kernel.ParseCommandDir(filepath.Join(dir, "commands"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range defs {
		d.InputSchema = contract.SchemaRef{ID: "journal.test." + d.Name + ".in", Version: 1}
		d.OutputSchema = contract.SchemaRef{ID: "journal.test." + d.Name + ".out", Version: 1}
		if regErr := cmds.Register(d); regErr != nil {
			t.Fatal(regErr)
		}
	}
	k := kernel.New(dialect, outbox.NewSQLWriter(dialect))
	k.Commands = cmds
	k.CommandSchemas = allowAllPOSSchemas{}
	actor := contract.ActorContext{PrincipalID: "Administrator", PrincipalType: contract.PrincipalHuman, Site: siteName, Roles: posAdminRoles, AuthenticatedAt: time.Now()}
	execution, err := policy.NewVerifiedExecution(contract.TenantID(siteName), actor)
	if err != nil {
		t.Fatal(err)
	}
	scope := contract.BusinessScope{TenantID: contract.TenantID(siteName), Kind: contract.ScopeKindTenant}
	post := func(lines []any) (map[string]any, *contract.Error) {
		raw, _ := json.Marshal(map[string]any{"data": map[string]any{"label": "Sales", "lines": lines}})
		oc := kernel.OperationContext{
			Site: siteName, User: "Administrator", UserRole: doctype.AdminRole, Roles: posAdminRoles, Actor: actor,
			Execution: execution, TenantID: contract.TenantID(siteName), BusinessScope: scope, Source: kernel.SourceIntegration,
		}
		res, cerr := k.Execute(ctx, conn, reg, kernel.Operation{Command: "journal.post_journal", Payload: raw, Context: oc})
		var out map[string]any
		_ = json.Unmarshal(res.Data, &out)
		return out, cerr
	}

	unbalanced := []any{
		map[string]any{"debit": 100, "credit": 0},
		map[string]any{"debit": 0, "credit": 50},
	}
	if _, e := post(unbalanced); e == nil || !strings.Contains(string(e.Details), "Debits and credits must be equal.") {
		t.Fatalf("unbalanced journal accepted: %+v", e)
	}

	balanced := []any{
		map[string]any{"debit": 100, "credit": 0},
		map[string]any{"debit": 0, "credit": 100},
	}
	out, e := post(balanced)
	if e != nil {
		t.Fatalf("balanced journal refused: %+v", e)
	}
	entry := reg.Get("Journal Entry")
	var debit, credit float64
	query := "SELECT total_debit, total_credit FROM " + dialect.QuoteIdent(entry.RawTableName()) + " WHERE name = ?"
	if err := conn.QueryRow(kdb.Rebind(dialect, query), journalReference(out)).Scan(&debit, &credit); err != nil {
		t.Fatalf("read stored totals: %v", err)
	}
	if debit != 100 || credit != 100 {
		t.Fatalf("stored totals = %v/%v, want 100/100", debit, credit)
	}
}

// journalReference extracts the created journal's name from the command output.
func journalReference(out map[string]any) string {
	output, _ := out["output"].(map[string]any)
	return fmt.Sprint(output["reference"])
}
