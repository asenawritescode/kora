//go:build integration

package site

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/metrics"
)

// The POS metric definitions survive activation and a read back from the stored config version.
func TestPOSMetricsRoundTripAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_POS_E2E_MYSQL_DSN"},
		{"postgres", "KORA_POS_E2E_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", tc.env, tc.driver)
			}
			conn, name := posDB(t, tc.driver, dsn)
			dialect := kdb.Resolve(tc.driver)
			if err := BootstrapSystemTables(context.Background(), conn, dialect); err != nil {
				t.Fatal(err)
			}
			const site = "metrics.example.test"
			var dts []*doctype.DocType // POS requires the inventory pack
			for _, pack := range []string{"inventory", "pos"} {
				parsed, err := doctype.ParseConfigTree(filepath.Join("..", "..", "config", pack))
				if err != nil {
					t.Fatal(err)
				}
				dts = append(dts, parsed...)
			}
			raw, _, err := metrics.LoadPack(filepath.Join("..", "..", "config", "pos"), dts)
			if err != nil {
				t.Fatal(err)
			}
			snap := &doctype.ConfigSnapshot{DocTypes: dts, Metrics: raw}
			if err = ImportConfigFromSnapshotForTenant(conn, doctype.NewRegistry(), name, site, site, "metrics round trip", snap, dialect, nil); err != nil {
				t.Fatal(err)
			}
			active, _, err := configstore.NewStore(conn, dialect).LoadActiveSnapshot(site)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, r := range active.Metrics {
				var m metrics.Metric
				if err := json.Unmarshal(r, &m); err != nil {
					t.Fatal(err)
				}
				got[m.Name] = true
			}
			for _, want := range []string{"sales", "receipts", "average_sale", "refunds"} {
				if !got[want] {
					t.Fatalf("active config lacks metric %q; got %v", want, got)
				}
			}
		})
	}
}
