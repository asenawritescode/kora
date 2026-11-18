//go:build integration

package tenanttime

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
)

// TestLoadReadsStoredSettings writes the two clock rows directly and reads them
// back with Load on a disposable database, for each dialect.
func TestLoadReadsStoredSettings(t *testing.T) {
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
			d := kdb.Resolve(tc.dialect)
			for _, ddl := range d.SystemTableSQL() {
				if !strings.HasPrefix(strings.TrimSpace(ddl), "CREATE TABLE IF NOT EXISTS") {
					continue
				}
				if _, err := database.DB.Exec(ddl); err != nil {
					t.Fatalf("system tables: %v", err)
				}
			}
			insert := kdb.Rebind(d, "INSERT INTO _kora_site_setting (site, setting_key, setting_value) VALUES (?, ?, ?)")
			for _, row := range [][]string{{"it-site", KeyTimeZone, "Europe/London"}, {"it-site", KeyBusinessDayCutoff, "03:00"}} {
				if _, err := database.DB.Exec(insert, row[0], row[1], row[2]); err != nil {
					t.Fatal(err)
				}
			}

			clock, err := Load(context.Background(), database.DB, d, "it-site")
			if err != nil {
				t.Fatal(err)
			}
			if clock.Location.String() != "Europe/London" || clock.Cutoff != 3*time.Hour {
				t.Fatalf("Load = %s %s", clock.Location, clock.Cutoff)
			}
		})
	}
}
