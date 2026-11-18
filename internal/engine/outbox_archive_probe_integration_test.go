//go:build integration

package engine

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/outbox"
)

// TestOutboxArchiveProbeAgainstDatabase checks the scanner's archive probe on a
// real database: old published rows wake the tenant, recent ones do not, and a
// zero horizon disables the probe.
func TestOutboxArchiveProbeAgainstDatabase(t *testing.T) {
	cases := []struct {
		name, driver, key, aged string
	}{
		{name: "mysql", driver: "mysql", key: "KORA_DURABLE_SCAN_DISPOSABLE_MYSQL_DSN", aged: "DATE_SUB(CURRENT_TIMESTAMP(6), INTERVAL %d DAY)"},
		{name: "postgres", driver: "postgres", key: "KORA_DURABLE_SCAN_DISPOSABLE_POSTGRES_DSN", aged: "NOW() - INTERVAL '%d days'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.key)
			if dsn == "" {
				t.Skipf("set %s to a fresh disposable database", tc.key)
			}
			database := disposabledb.Open(t, tc.driver, dsn).DB
			dialect := db.Resolve(tc.driver)
			for _, statement := range outboxSchema(tc.driver) {
				if _, err := database.Exec(statement); err != nil {
					t.Fatalf("create outbox fixture: %v\n%s", err, statement)
				}
			}
			insert := func(id string, days int) {
				query := `INSERT INTO _kora_outbox (id,site,event_type,status,created_at) VALUES (?,?,?,'published',` + fmt.Sprintf(tc.aged, days) + `)`
				if _, err := database.Exec(db.Rebind(dialect, query), id, "probe-test", "kora.test.created"); err != nil {
					t.Fatal("insert outbox row:", err)
				}
			}
			probe := func(horizon time.Duration) bool {
				due, err := hasArchiveDueOutbox(t.Context(), database, dialect, horizon)
				if err != nil {
					t.Fatal("archive probe:", err)
				}
				return due
			}
			insert("recent", 1)
			if probe(outbox.DefaultArchiveHorizon) {
				t.Fatal("probe reported archive work for a row one day old")
			}
			insert("old", 500)
			if !probe(outbox.DefaultArchiveHorizon) {
				t.Fatal("probe missed a row 500 days old")
			}
			if probe(0) {
				t.Fatal("probe ran with the archive disabled")
			}
		})
	}
}
