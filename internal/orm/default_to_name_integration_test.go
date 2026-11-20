//go:build integration

package orm_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
)

// A record type with a numbering series and an optional, unique reference
// marked default_to_name behaves the same on MySQL and PostgreSQL: the flag
// survives the field store, a blank or missing reference takes the record's
// number, a typed one is kept and stays unique, and clearing it on update gives
// the number back. Run against disposable servers:
// KORA_ORM_DISPOSABLE_MYSQL_DSN and KORA_ORM_DISPOSABLE_POSTGRES_DSN.
func TestDefaultToNameAcrossDialects(t *testing.T) {
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
			createStoreTables(t, database, dialect)
			for _, statement := range configstore.ConfigRevisionSchema(dialect) {
				if _, err := database.DB.Exec(statement); err != nil {
					t.Fatalf("create config revision table: %v", err)
				}
			}
			if err := configstore.SeedConfigRevision(context.Background(), database.DB, dialect); err != nil {
				t.Fatalf("seed config revision: %v", err)
			}

			trip, err := doctype.ParseYAML([]byte(`name: Trip
naming_series: TRIP-.YYYY.-.#####
module: Fleet
title_field: reference
fields:
  - fieldname: reference
    fieldtype: Data
    default_to_name: true
    unique: true
    search_index: true
  - fieldname: origin
    fieldtype: Data
`), "trip.yaml")
			if err != nil {
				t.Fatal(err)
			}
			store := configstore.NewStore(database.DB, dialect)
			if err := store.SaveDocType(trip, ""); err != nil {
				t.Fatalf("save record type: %v", err)
			}
			loaded, err := store.LoadAll("")
			if err != nil {
				t.Fatalf("load record types: %v", err)
			}
			var stored *doctype.DocType
			for _, dt := range loaded {
				if dt.Name == "Trip" {
					stored = dt
				}
			}
			if stored == nil || stored.GetField("reference") == nil || !stored.GetField("reference").DefaultToName {
				t.Fatal("default_to_name was lost in the field store")
			}

			registry := doctype.NewRegistry()
			registry.Register(stored)
			if _, err := database.DB.Exec(createTripTable(dialect)); err != nil {
				t.Fatalf("create trip table: %v", err)
			}
			// The unique index is the Engine's own, as on a real workspace.
			if _, err := database.DB.Exec(dialect.CreateIndex("tabTrip", "reference", true)); err != nil {
				t.Fatalf("create unique index: %v", err)
			}
			manager := &orm.TxManager{DB: database.DB, Registry: registry, Dialect: dialect}
			write := func(name string, fields map[string]any) (*doctype.Document, error) {
				doc := doctype.NewDocument("Trip")
				doc.Name = name
				for k, v := range fields {
					doc.Set(k, v)
				}
				dbTx, err := database.DB.Begin()
				if err != nil {
					t.Fatal(err)
				}
				if name != "" {
					err = manager.SaveInTxWithScope(dbTx, stored, doc, "u1", policy.RowScope{}, nil)
				} else {
					err = manager.InsertInTx(dbTx, stored, doc, "u1", "u1")
				}
				if err != nil {
					_ = dbTx.Rollback()
					return nil, err
				}
				return doc, dbTx.Commit()
			}
			year := time.Now().UTC().Format("2006")
			var typed *doctype.Document
			for _, tc := range []struct {
				label  string
				fields map[string]any
				want   string // "" = the record's own number
			}{
				{"blank", map[string]any{"reference": ""}, ""},
				{"blank again", map[string]any{"reference": "  "}, ""},
				{"missing", map[string]any{"origin": "Nairobi"}, ""},
				{"typed", map[string]any{"reference": "PO-7781"}, "PO-7781"},
			} {
				doc, err := write("", tc.fields)
				if err != nil {
					t.Fatalf("%s: insert: %v", tc.label, err)
				}
				if !strings.HasPrefix(doc.Name, "TRIP-"+year+"-") {
					t.Fatalf("%s: name %q is not from the TRIP-%s- series", tc.label, doc.Name, year)
				}
				want := tc.want
				if want == "" {
					want = doc.Name
				}
				if got := storedReference(t, database, dialect, doc.Name); got != want {
					t.Fatalf("%s: stored reference = %q, want %q", tc.label, got, want)
				}
				if tc.want != "" {
					typed = doc
				}
			}
			if _, err := write("", map[string]any{"reference": "PO-7781"}); !errors.Is(err, orm.ErrDuplicate) {
				t.Fatalf("a typed reference used twice was not refused: %v", err)
			}
			if _, err := write(typed.Name, map[string]any{"reference": ""}); err != nil {
				t.Fatalf("clear the reference: %v", err)
			}
			if got := storedReference(t, database, dialect, typed.Name); got != typed.Name {
				t.Fatalf("a cleared reference = %q, want the record's number %q", got, typed.Name)
			}
		})
	}
}

// createStoreTables creates the field store tables with the dialect's own DDL,
// including the columns later releases add. ADD COLUMN for a column the CREATE
// already has fails and is skipped, as bootstrap does.
func createStoreTables(t *testing.T, database *disposabledb.Database, dialect db.Dialect) {
	t.Helper()
	for _, statement := range dialect.SystemTableSQL() {
		if !strings.Contains(statement, "_kora_field") && !strings.Contains(statement, "_kora_doctype") && !strings.Contains(statement, "_kora_naming_series") {
			continue
		}
		if _, err := database.DB.Exec(statement); err != nil && !strings.Contains(strings.ToUpper(statement), "ALTER TABLE") && !strings.Contains(strings.ToUpper(statement), "CREATE INDEX") {
			t.Fatalf("create field store: %v\n%s", err, statement)
		}
	}
}

func createTripTable(dialect db.Dialect) string {
	table := dialect.QuoteIdent("tabTrip")
	return "CREATE TABLE " + table + ` (name VARCHAR(140) PRIMARY KEY, owner VARCHAR(140), creation TIMESTAMP NULL, modified TIMESTAMP NULL,
		modified_by VARCHAR(140), doc_status INT, idx INT, revision INT NOT NULL DEFAULT 1,
		reference VARCHAR(140), origin VARCHAR(140))`
}

func storedReference(t *testing.T, database *disposabledb.Database, dialect db.Dialect, name string) string {
	t.Helper()
	var reference string
	query := db.Rebind(dialect, "SELECT reference FROM "+dialect.QuoteIdent("tabTrip")+" WHERE name = ?")
	if err := database.DB.QueryRow(query, name).Scan(&reference); err != nil {
		t.Fatalf("read reference of %s: %v", name, err)
	}
	return reference
}
