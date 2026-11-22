//go:build integration

package facts

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
)

// TestGeneratedModelDriftSettlesAfterOneRecompute registers two generated fact models with rows (Register with a
// Link, a Check, a Select and a Float; Location with a Data field only) and runs the drift check. The first
// cycle finds the empty facts and summaries, queues the repair and refreshes; the next cycle must report no drift.
func TestGeneratedModelDriftSettlesAfterOneRecompute(t *testing.T) {
	tests := []struct {
		driver, envVar string
	}{
		{"mysql", "KORA_FACTS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_FACTS_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range tests {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run", tc.envVar)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			dialect := db.Resolve(tc.driver)
			ctx := context.Background()
			q := dialect.QuoteIdent
			exec := func(s string, args ...any) {
				t.Helper()
				if _, err := database.DB.ExecContext(ctx, db.Rebind(dialect, s), args...); err != nil {
					t.Fatalf("%s: %v", s, err)
				}
			}
			exec("CREATE TABLE " + q("tabRegister") + " (name VARCHAR(140) NOT NULL PRIMARY KEY, owner VARCHAR(140) NOT NULL, creation TIMESTAMP(6) NOT NULL, opening_balance DECIMAL(21,9) NOT NULL, location VARCHAR(140) NOT NULL, enabled INT NOT NULL)")
			exec("CREATE TABLE " + q("tabLocation") + " (name VARCHAR(140) NOT NULL PRIMARY KEY, owner VARCHAR(140) NOT NULL, creation TIMESTAMP(6) NOT NULL, town VARCHAR(140) NOT NULL)")
			exec("CREATE TABLE _kora_tenant_event_sequence (id INT PRIMARY KEY, last_sequence BIGINT NOT NULL DEFAULT 0)")
			exec("INSERT INTO _kora_tenant_event_sequence (id, last_sequence) VALUES (1, 1)")
			exec("CREATE TABLE _kora_site_setting (site VARCHAR(140) NOT NULL, setting_key VARCHAR(140) NOT NULL, setting_value VARCHAR(140) NOT NULL, PRIMARY KEY (site, setting_key))")
			for _, n := range []string{"R1", "R2", "R3"} {
				exec("INSERT INTO "+q("tabRegister")+" (name, owner, creation, opening_balance, location, enabled) VALUES (?, 'u1', '2026-11-18 10:00:00.123456', 5.5, 'L1', 1)", n)
			}
			for _, n := range []string{"L1", "L2", "L3"} {
				exec("INSERT INTO "+q("tabLocation")+" (name, owner, creation, town) VALUES (?, 'u1', '2026-11-18 10:00:00', 'Nairobi')", n)
			}
			reg := doctype.NewRegistry()
			reg.Register(&doctype.DocType{Name: "Location", Fields: []doctype.Field{{Fieldname: "town", Fieldtype: "Data"}}})
			reg.Register(&doctype.DocType{Name: "Register", Fields: []doctype.Field{
				{Fieldname: "opening_balance", Fieldtype: "Currency"}, {Fieldname: "location", Fieldtype: "Link", Options: "Location"},
				{Fieldname: "enabled", Fieldtype: "Check"}}})
			if err := (&ModelSync{DB: database.DB, Dialect: dialect, Registry: reg, Owner: "sync"}).Sync(ctx); err != nil {
				t.Fatal(err)
			}
			checker := &DriftChecker{DB: database.DB, Dialect: dialect, Registry: reg, Site: "test", Admission: openLane{}, Owner: "checker-a",
				Now: func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) }}
			refresher := &Refresher{DB: database.DB, Dialect: dialect, Registry: reg, Admission: openLane{}, Owner: "refresher-a"}

			if n, err := checker.RunDue(ctx); err != nil || n != 2 {
				t.Fatalf("first cycle drifted %d (%v), want 2 (both models: facts and summaries are empty)", n, err)
			}
			if _, err := refresher.RefreshDue(ctx); err != nil {
				t.Fatal(err)
			}
			if n, err := checker.RunDue(ctx); err != nil || n != 0 {
				t.Fatalf("cycle after one recompute drifted %d (%v), want 0", n, err)
			}
			for name, want := range map[string]int{"Register": 3, "Location": 3} {
				dt := reg.Get(name)
				if got := countRows(t, database.DB, dialect, "SELECT COUNT(*) FROM "+q(dt.Facts.TableName(dt))); got != want {
					t.Errorf("%s facts = %d, want %d", name, got, want)
				}
				if got := countRows(t, database.DB, dialect, "SELECT COUNT(*) FROM "+q(summaryTable(dt))); got == 0 {
					t.Errorf("%s summary is empty after the recompute", name)
				}
			}
		})
	}
}
