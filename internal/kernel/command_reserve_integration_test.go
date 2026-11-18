//go:build integration

package kernel_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/schema"
)

const reserveNumbersYAML = `
name: reserve_numbers
namespace: pos
version: 1
input:
  record: Till Session
  extra:
    till_session: {type: string, required: true}
transaction:
  - as: block
    reserve: {record: Sale, owner: $input.till_session, size: 50}
output:
  reference: $steps.block.name
`

// newReserveFixture builds a site with Sale and Till Session on the given
// dialect. It is a separate fixture because newSite is MySQL-only.
func newReserveFixture(t *testing.T, driver, dsnEnv string) *siteFixture {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(dsnEnv))
	if dsn == "" {
		t.Skipf("set %s to a disposable %s server", dsnEnv, driver)
	}
	database := disposabledb.Open(t, driver, dsn)
	dialect := db.Resolve(driver)
	outboxDDL, kernelDDL := db.OutboxTablesMySQL(), db.KernelTablesMySQL()
	if driver == "postgres" {
		outboxDDL, kernelDDL = db.OutboxTablesPostgres(), db.KernelTablesPostgres()
	}
	var statements []string
	for _, ddl := range dialect.SystemTableSQL() {
		if strings.HasPrefix(strings.TrimSpace(ddl), "CREATE TABLE IF NOT EXISTS") {
			statements = append(statements, ddl)
		}
	}
	statements = append(append(statements, outboxDDL...), kernelDDL...)
	statements = append(statements, configstore.ConfigRevisionSchema(dialect)...)
	if err := dialect.ExecuteBatch(database.DB, statements); err != nil {
		t.Fatalf("system tables: %v", err)
	}
	if err := configstore.SeedConfigRevision(context.Background(), database.DB, dialect); err != nil {
		t.Fatalf("seed config revision: %v", err)
	}

	saleDT := &doctype.DocType{
		Name: "Sale", Module: "Kernel Test", NamingSeries: "SALE-.YYYY.-.#####",
		Fields: []doctype.Field{{Fieldname: "register", Fieldtype: "Data", Label: "Register"}},
	}
	sessionDT := &doctype.DocType{
		Name: "Till Session", Module: "Kernel Test",
		Fields: []doctype.Field{{Fieldname: "status", Fieldtype: "Data", Label: "Status"}},
	}
	roles := []*doctype.Role{{Name: "Administrator"}}
	perms := []*doctype.Permission{
		{Doctype: "Sale", Role: "Administrator", Read: true, Write: true, Create: true, Delete: true},
		{Doctype: "Till Session", Role: "Administrator", Read: true, Write: true, Create: true, Delete: true},
	}
	reg := doctype.NewRegistry()
	reg.LoadFull([]*doctype.DocType{saleDT, sessionDT}, roles, perms)
	if err := schema.MigrateSiteFromRegistry(database.DB, database.Name, reg, dialect); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &siteFixture{DB: database.DB, Name: database.Name, Registry: reg, Dialect: dialect, StatusPolicies: &testStatusPolicyRepository{}}
}

// TestReserveNumbersCommandGivesEachSessionDisjointOpenBlocks runs the
// reserve_numbers step through the kernel for two till sessions and checks
// the recorded blocks: fifty numbers each, open, and never overlapping.
func TestReserveNumbersCommandGivesEachSessionDisjointOpenBlocks(t *testing.T) {
	cases := []struct{ driver, env string }{
		{"mysql", "KORA_KERNEL_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_KERNEL_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range cases {
		t.Run(tc.driver, func(t *testing.T) {
			s := newReserveFixture(t, tc.driver, tc.env)
			defer s.DB.Close()
			k := newKernel(s)
			k.Commands = kernel.NewCommandRegistry()
			mustRegister(t, k, reserveNumbersYAML)

			for _, session := range []string{"till-session-1", "till-session-2"} {
				payload, _ := json.Marshal(map[string]any{"data": map[string]any{"till_session": session}})
				res := exec(t, s, k, kernel.Operation{Command: "pos.reserve_numbers", Payload: payload, Context: opCtx(s.Name, "Administrator")})
				mustComplete(t, res)
			}

			rows, err := s.DB.Query("SELECT owner, first_number, last_number, state FROM _kora_number_block ORDER BY first_number")
			if err != nil {
				t.Fatalf("read blocks: %v", err)
			}
			defer rows.Close()
			type block struct {
				owner, state string
				first, last  int64
			}
			var blocks []block
			for rows.Next() {
				var b block
				if err := rows.Scan(&b.owner, &b.first, &b.last, &b.state); err != nil {
					t.Fatalf("scan block: %v", err)
				}
				blocks = append(blocks, b)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if len(blocks) != 2 {
				t.Fatalf("recorded %d blocks, want 2", len(blocks))
			}
			for i, b := range blocks {
				if b.last-b.first+1 != 50 || b.state != "open" {
					t.Fatalf("block %d = %+v, want fifty numbers in state open", i, b)
				}
			}
			if blocks[0].owner != "till-session-1" || blocks[1].owner != "till-session-2" {
				t.Fatalf("owners = %q, %q", blocks[0].owner, blocks[1].owner)
			}
			if blocks[0].last >= blocks[1].first {
				t.Fatalf("blocks overlap: %+v and %+v", blocks[0], blocks[1])
			}
		})
	}
}
