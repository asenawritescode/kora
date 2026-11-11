//go:build integration

package schema

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
)

// A unique field added to a table that already exists must be enforced by the database, as
// it is when the table is created with it. Run against disposable servers: KORA_SCHEMA_MYSQL_DSN
// and KORA_SCHEMA_POSTGRES_DSN.
func TestUniqueFieldAddedToExistingTableGetsItsIndex(t *testing.T) {
	for _, c := range []struct{ driver, env string }{{"mysql", "KORA_SCHEMA_MYSQL_DSN"}, {"postgres", "KORA_SCHEMA_POSTGRES_DSN"}} {
		t.Run(c.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(c.env))
			if dsn == "" {
				t.Skip("set " + c.env + " to a disposable server")
			}
			database := disposabledb.Open(t, c.driver, dsn)
			dialect := db.Resolve(c.driver)
			plan := func(dt *doctype.DocType) []string {
				t.Helper()
				registry := doctype.NewRegistry()
				registry.Register(dt)
				statements, err := PlanSnapshotActivation(database.DB, database.Name, dialect, registry, &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{dt}}, "", "rev")
				if err != nil {
					t.Fatalf("plan: %v", err)
				}
				return statements
			}
			apply := func(dt *doctype.DocType) {
				t.Helper()
				if err := ApplySnapshotActivationDDL(context.Background(), database.DB, dialect, plan(dt)); err != nil {
					t.Fatalf("apply: %v", err)
				}
			}
			// Version one has no reference; version two adds it, unique but not search-indexed.
			before := &doctype.DocType{Name: "Sale", Fields: []doctype.Field{{Fieldname: "note", Fieldtype: "Data"}}}
			after := &doctype.DocType{Name: "Sale", Fields: []doctype.Field{
				{Fieldname: "note", Fieldtype: "Data"},
				{Fieldname: "reference", Fieldtype: "Data", Unique: true},
			}}
			apply(before)
			apply(after)

			live, err := LoadLiveSchema(database.DB, database.Name, dialect)
			if err != nil {
				t.Fatal(err)
			}
			column := live[after.RawTableName()].Columns["reference"]
			if column == nil || len(column.UniqueIndexNames) == 0 {
				t.Fatalf("reference has no unique index after the upgrade: %+v", column)
			}
			if again := plan(after); len(again) != 0 {
				t.Fatalf("a second plan still has work to do: %v", again)
			}
		})
	}
}
