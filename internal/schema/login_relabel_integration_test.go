//go:build integration

package schema

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
)

// Data holding logins becomes User without a conversion: the impact preview has no blocked change, no DDL is
// planned, and the rows written under Data still hold the logins. Run against disposable servers:
// KORA_SCHEMA_MYSQL_DSN and KORA_SCHEMA_POSTGRES_DSN.
func TestDataToUserRelabelIsNotBlockedAndKeepsLogins(t *testing.T) {
	for _, c := range []struct{ driver, env string }{{"mysql", "KORA_SCHEMA_MYSQL_DSN"}, {"postgres", "KORA_SCHEMA_POSTGRES_DSN"}} {
		t.Run(c.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(c.env))
			if dsn == "" {
				t.Skip("set " + c.env + " to a disposable server")
			}
			database := disposabledb.Open(t, c.driver, dsn)
			dialect := db.Resolve(c.driver)
			ctx := context.Background()
			registry := doctype.NewRegistry()
			till := func(cashierType string) *doctype.DocType {
				return &doctype.DocType{Name: "Till Session", Fields: []doctype.Field{{Fieldname: "cashier", Fieldtype: cashierType}}}
			}
			before, after := till("Data"), till("User")
			registry.Register(before)
			plan, err := PlanSnapshotActivation(database.DB, database.Name, dialect, registry, &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{before}}, "", "rev-before")
			if err != nil {
				t.Fatalf("plan before: %v", err)
			}
			if applyErr := ApplySnapshotActivationDDL(ctx, database.DB, dialect, plan); applyErr != nil {
				t.Fatalf("apply before: %v", applyErr)
			}
			table := dialect.QuoteIdent(before.RawTableName())
			if _, err = database.DB.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (name, cashier) VALUES ('TS-1', 'ana.login')", table)); err != nil {
				t.Fatalf("seed old Data cashier: %v", err)
			}

			preview := AnalyzeImpact(database.DB, before, after, registry, dialect)
			if len(preview.Blocked) != 0 {
				t.Fatalf("Data to User is blocked: %+v", preview.Blocked)
			}
			if len(preview.DDL) != 0 {
				t.Fatalf("Data to User plans DDL: %v", preview.DDL)
			}

			upgrade, err := PlanSnapshotActivation(database.DB, database.Name, dialect, registry, &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{after}}, "", "rev-after")
			if err != nil {
				t.Fatalf("plan after: %v", err)
			}
			if len(upgrade) != 0 {
				t.Fatalf("upgrade to User plans statements over the live table: %v", upgrade)
			}

			var cashier string
			if err := database.DB.QueryRowContext(ctx, fmt.Sprintf("SELECT cashier FROM %s WHERE name = 'TS-1'", table)).Scan(&cashier); err != nil {
				t.Fatal(err)
			}
			if cashier != "ana.login" {
				t.Fatalf("seeded login changed to %q", cashier)
			}
		})
	}
}
