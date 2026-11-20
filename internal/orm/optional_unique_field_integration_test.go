//go:build integration

package orm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/schema"
)

// A field that a package upgrade makes optional must accept records without a
// value on an existing workspace, as it does on a new one: the column becomes
// nullable, and blank values do not collide on its unique index. Run against
// disposable servers: KORA_ORM_DISPOSABLE_MYSQL_DSN and KORA_ORM_DISPOSABLE_POSTGRES_DSN.
func TestOptionalUniqueFieldOnExistingTable(t *testing.T) {
	for _, c := range []struct{ driver, env string }{
		{"mysql", "KORA_ORM_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_ORM_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(c.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(c.env))
			if dsn == "" {
				t.Skip("set " + c.env + " to a disposable server")
			}
			database := disposabledb.Open(t, c.driver, dsn)
			dialect := db.Resolve(c.driver)
			member := func(reqd bool) *doctype.DocType {
				return &doctype.DocType{Name: "Member", Fields: []doctype.Field{
					{Fieldname: "label", Fieldtype: "Data"},
					{Fieldname: "code", Fieldtype: "Data", Reqd: reqd, Unique: true},
				}}
			}
			plan := func(dt *doctype.DocType) []string {
				t.Helper()
				registry := doctype.NewRegistry()
				registry.Register(dt)
				statements, err := schema.PlanSnapshotActivation(database.DB, database.Name, dialect, registry, &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{dt}}, "", "rev")
				if err != nil {
					t.Fatalf("plan: %v", err)
				}
				return statements
			}
			apply := func(dt *doctype.DocType) {
				t.Helper()
				if err := schema.ApplySnapshotActivationDDL(context.Background(), database.DB, dialect, plan(dt)); err != nil {
					t.Fatalf("apply: %v", err)
				}
			}
			codeNullable := func() bool {
				t.Helper()
				live, err := schema.LoadLiveSchema(database.DB, database.Name, dialect)
				if err != nil {
					t.Fatal(err)
				}
				return live["tabMember"].Columns["code"].Nullable
			}

			apply(member(true))
			if codeNullable() {
				t.Fatal("a required field's column is nullable")
			}
			optional := member(false)
			apply(optional)
			if !codeNullable() {
				t.Fatal("the column stayed NOT NULL after the field became optional")
			}
			if again := plan(optional); len(again) != 0 {
				t.Fatalf("a second plan still has work to do: %v", again)
			}

			registry := doctype.NewRegistry()
			registry.Register(optional)
			manager := &TxManager{DB: database.DB, Registry: registry, Dialect: dialect}
			write := func(name string, fields map[string]any, update bool) error {
				doc := doctype.NewDocument("Member")
				doc.Name = name
				for k, v := range fields {
					doc.Set(k, v)
				}
				dbTx, err := database.DB.Begin()
				if err != nil {
					t.Fatal(err)
				}
				if update {
					err = manager.SaveInTxWithScope(dbTx, optional, doc, "u1", policy.RowScope{}, nil)
				} else {
					err = manager.InsertInTx(dbTx, optional, doc, "u1", "u1")
				}
				if err != nil {
					_ = dbTx.Rollback()
					return err
				}
				return dbTx.Commit()
			}
			for _, tc := range []struct {
				name   string
				fields map[string]any
			}{
				{"M-1", map[string]any{"label": "omitted"}},
				{"M-2", map[string]any{"label": "omitted"}},
				{"M-3", map[string]any{"label": "blank", "code": ""}},
				{"M-4", map[string]any{"label": "blank", "code": ""}},
				{"M-5", map[string]any{"label": "real", "code": "A-1"}},
			} {
				if err := write(tc.name, tc.fields, false); err != nil {
					t.Fatalf("insert %s: %v", tc.name, err)
				}
			}
			if err := write("M-6", map[string]any{"code": "A-1"}, false); !errors.Is(err, ErrDuplicate) {
				t.Fatalf("a second real value was not rejected as a duplicate: %v", err)
			}
			if err := write("M-5", map[string]any{"label": "cleared", "code": ""}, true); err != nil {
				t.Fatalf("update to blank: %v", err)
			}
			var nulls int
			query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE code IS NULL", dialect.QuoteIdent("tabMember"))
			if err := database.DB.QueryRow(query).Scan(&nulls); err != nil {
				t.Fatal(err)
			}
			if nulls != 5 {
				t.Fatalf("%d rows store code as NULL, want 5 (omitted, blank and cleared)", nulls)
			}
		})
	}
}
