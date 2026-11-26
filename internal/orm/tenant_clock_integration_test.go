//go:build integration

package orm

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/schema"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// TestDateTodayDefaultUsesTenantClock inserts a "today" Date row for two sites
// whose zones are 26 hours apart (UTC+14 and UTC-12), so their business days
// always differ. Each stored date must equal that site's business day.
func TestDateTodayDefaultUsesTenantClock(t *testing.T) {
	for _, tc := range []struct{ name, dialect, env string }{
		{"mysql", "mysql", "KORA_TENANT_CLOCK_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_TENANT_CLOCK_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			database := disposabledb.Open(t, tc.dialect, dsn)
			conn := database.DB
			d := kdb.Resolve(tc.dialect)
			for _, ddl := range d.SystemTableSQL() {
				if !strings.HasPrefix(strings.TrimSpace(ddl), "CREATE TABLE IF NOT EXISTS") {
					continue
				}
				if _, err := conn.Exec(ddl); err != nil {
					t.Fatalf("system tables: %v", err)
				}
			}
			// CREATE TABLE must accept a "today" Date default (MySQL rejects
			// DEFAULT CURRENT_DATE); the ORM fills the day in the tenant zone.
			fields := []doctype.Field{
				{Fieldname: "code", Fieldtype: "Data", Label: "Code"},
				{Fieldname: "visited_on", Fieldtype: "Date", Label: "Visited on", Default: "today"},
			}
			dt := &doctype.DocType{Name: "Visit", Module: "T", Fields: fields}
			schemaReg := doctype.NewRegistry()
			schemaReg.LoadFull([]*doctype.DocType{dt}, nil, nil)
			if err := schema.MigrateSiteFromRegistry(conn, "", schemaReg, d); err != nil {
				t.Fatalf("create table with today default: %v", err)
			}
			reg := doctype.NewRegistry()
			reg.LoadFull([]*doctype.DocType{dt}, nil, nil)

			kiritimati := insertVisitForZone(t, conn, d, reg, dt, "it-kiritimati", "Pacific/Kiritimati", "V-1")
			gmtMinus12 := insertVisitForZone(t, conn, d, reg, dt, "it-gmt-minus-12", "Etc/GMT+12", "V-2")
			if kiritimati.stored == gmtMinus12.stored {
				t.Fatalf("both zones stored %s; the test needs different business days", kiritimati.stored)
			}
			for _, got := range []zoneVisit{kiritimati, gmtMinus12} {
				if got.stored != got.want && got.stored != got.wantAfter {
					t.Fatalf("site %s visited_on = %s, want business day %s", got.site, got.stored, got.want)
				}
			}
		})
	}
}

type zoneVisit struct {
	site, stored, want, wantAfter string
}

// insertVisitForZone stores the zone on a fresh site, inserts one visit with a
// "today" default, and returns the stored date with the expected business day.
// The expected day is read before and after the insert: a midnight boundary
// between the two may legitimately move it, so either is accepted.
func insertVisitForZone(t *testing.T, conn *sql.DB, d kdb.Dialect, reg *doctype.Registry, dt *doctype.DocType, site, zone, code string) zoneVisit {
	t.Helper()
	if _, err := conn.Exec(kdb.Rebind(d,
		"INSERT INTO _kora_site_setting (site, setting_key, setting_value) VALUES (?, ?, ?)"),
		site, tenanttime.KeyTimeZone, zone); err != nil {
		t.Fatal(err)
	}
	before := mustClock(t, conn, d, site).BusinessDate(time.Now()).String()
	tx := &TxManager{DB: conn, Registry: reg, Dialect: d, SiteName: site}
	doc := doctype.NewDocument("Visit")
	doc.Set("code", code)
	if err := insertDoc(conn, tx, dt, doc); err != nil {
		t.Fatal(err)
	}
	after := mustClock(t, conn, d, site).BusinessDate(time.Now()).String()
	return zoneVisit{
		site:      site,
		stored:    readVisitedOn(t, conn, d, doc.Name),
		want:      before,
		wantAfter: after,
	}
}

func insertDoc(conn *sql.DB, tx *TxManager, dt *doctype.DocType, doc *doctype.Document) error {
	dbTx, err := conn.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if err := tx.PrepareInsert(dbTx, dt, doc); err != nil {
		_ = dbTx.Rollback()
		return err
	}
	if err := tx.InsertInTx(dbTx, dt, doc, "u1", "u1"); err != nil {
		_ = dbTx.Rollback()
		return err
	}
	return dbTx.Commit()
}

func mustClock(t *testing.T, conn *sql.DB, d kdb.Dialect, site string) tenanttime.Clock {
	t.Helper()
	clock, err := tenanttime.Load(context.Background(), conn, d, site)
	if err != nil {
		t.Fatal(err)
	}
	return clock
}

func readVisitedOn(t *testing.T, conn *sql.DB, d kdb.Dialect, name string) string {
	t.Helper()
	var got time.Time
	query := kdb.Rebind(d, "SELECT visited_on FROM "+d.QuoteIdent("tabVisit")+" WHERE name = ?")
	if err := conn.QueryRow(query, name).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got.Format(time.DateOnly)
}

// TestDateTodayDefaultAddColumn adds a "today" Date field to an existing table
// (ADD COLUMN, not CREATE TABLE). The statement must succeed for a nullable and
// a required field, and an ORM insert afterwards must fill the tenant's day.
func TestDateTodayDefaultAddColumn(t *testing.T) {
	for _, tc := range []struct{ name, dialect, env string }{
		{"mysql", "mysql", "KORA_TENANT_CLOCK_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_TENANT_CLOCK_DISPOSABLE_POSTGRES_DSN"},
	} {
		for _, reqd := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reqd=%v", tc.name, reqd), func(t *testing.T) {
				dsn := os.Getenv(tc.env)
				if dsn == "" {
					t.Skipf("set %s", tc.env)
				}
				conn := disposabledb.Open(t, tc.dialect, dsn).DB
				d := kdb.Resolve(tc.dialect)
				for _, ddl := range d.SystemTableSQL() {
					if strings.HasPrefix(strings.TrimSpace(ddl), "CREATE TABLE IF NOT EXISTS") {
						if _, err := conn.Exec(ddl); err != nil {
							t.Fatalf("system tables: %v", err)
						}
					}
				}
				base := []doctype.Field{{Fieldname: "code", Fieldtype: "Data", Label: "Code"}}
				before := doctype.NewRegistry()
				before.LoadFull([]*doctype.DocType{{Name: "Visit", Module: "T", Fields: base}}, nil, nil)
				if err := schema.MigrateSiteFromRegistry(conn, "", before, d); err != nil {
					t.Fatalf("create table: %v", err)
				}

				fields := append(append([]doctype.Field{}, base...),
					doctype.Field{Fieldname: "visited_on", Fieldtype: "Date", Label: "Visited on", Default: "today", Reqd: reqd})
				dt := &doctype.DocType{Name: "Visit", Module: "T", Fields: fields}
				reg := doctype.NewRegistry()
				reg.LoadFull([]*doctype.DocType{dt}, nil, nil)
				if err := schema.MigrateSiteFromRegistry(conn, "", reg, d); err != nil {
					t.Fatalf("add column with today default: %v", err)
				}

				visit := insertVisitForZone(t, conn, d, reg, dt, "it-add-column", "Pacific/Kiritimati", "V-3")
				if visit.stored != visit.want && visit.stored != visit.wantAfter {
					t.Fatalf("visited_on = %s, want business day %s", visit.stored, visit.want)
				}
			})
		}
	}
}
