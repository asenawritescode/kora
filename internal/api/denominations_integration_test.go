//go:build integration

package api

import (
	"net/http"
	"os"
	"testing"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
)

// TestDenominationsRoundTripOnDisposableDatabases stores a KES list through the
// settings PUT, reads it back through readSiteSettings, and checks that an
// invalid list is refused without touching the stored one.
func TestDenominationsRoundTripOnDisposableDatabases(t *testing.T) {
	for _, tc := range []struct{ name, dialect, env string }{
		{"mysql", "mysql", "KORA_SITE_SETTINGS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_SITE_SETTINGS_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			database := disposabledb.Open(t, tc.dialect, dsn)
			for _, ddl := range createTableStatements(kdb.Resolve(tc.dialect).SystemTableSQL()) {
				if _, err := database.DB.Exec(ddl); err != nil {
					t.Fatalf("create system tables: %v", err)
				}
			}
			h := NewHandler(doctype.NewRegistry(), nil)

			c, w := settingsContext(database.DB, tc.dialect, `{"currency":"KES","denominations":[{"value":500,"kind":"note"},{"value":"0.5","kind":"COIN"}]}`)
			h.HandleSiteSettingsUpdate(c)
			if w.Code != http.StatusOK {
				t.Fatalf("PUT denominations = %d: %s", w.Code, w.Body.String())
			}

			c, _ = settingsContext(database.DB, tc.dialect, "")
			got, err := h.readSiteSettings(c)
			if err != nil {
				t.Fatal(err)
			}
			lines, ok := got["denominations"].([]denomination)
			if !ok || len(lines) != 2 || lines[0].Value != "500" || lines[0].Kind != kindNote || lines[1].Value != "0.5" || lines[1].Kind != kindCoin {
				t.Fatalf("denominations = %#v", got["denominations"])
			}

			c, w = settingsContext(database.DB, tc.dialect, `{"denominations":[{"value":"5","kind":"coin"},{"value":"10","kind":"coin"}]}`)
			h.HandleSiteSettingsUpdate(c)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("unsorted PUT = %d, want 400: %s", w.Code, w.Body.String())
			}
			c, _ = settingsContext(database.DB, tc.dialect, "")
			again, err := h.readSiteSettings(c)
			if err != nil {
				t.Fatal(err)
			}
			if len(again["denominations"].([]denomination)) != 2 {
				t.Fatalf("refused PUT changed the stored list: %#v", again["denominations"])
			}
		})
	}
}
