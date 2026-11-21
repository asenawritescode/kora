//go:build integration

package metrics_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/metrics"
	"github.com/asenawritescode/kora/internal/site"
)

// A dashboard survives activation and a read back from the stored config version, on MySQL and Postgres.
func TestDashboardSnapshotPersistsAcrossDialects(t *testing.T) {
	tests := []struct{ driver, envVar string }{
		{"mysql", "KORA_METRICS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_METRICS_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range tests {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run", tc.envVar)
			}
			fixture := disposabledb.Open(t, tc.driver, dsn)
			dialect := kdb.Resolve(tc.driver)
			if err := site.BootstrapSystemTables(context.Background(), fixture.DB, dialect); err != nil {
				t.Fatal(err)
			}
			var dts []*doctype.DocType
			for _, pack := range []string{"inventory", "pos"} {
				parsed, err := doctype.ParseConfigTree(filepath.Join("..", "..", "config", pack))
				if err != nil {
					t.Fatal(err)
				}
				dts = append(dts, parsed...)
			}
			defs, err := metrics.ParseDirectory(filepath.Join("..", "..", "config", "pos", "metrics"))
			if err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile("testdata/till_insights.yaml")
			if err != nil {
				t.Fatal(err)
			}
			d, err := metrics.ParseDashboardYAML(body, "till_insights.yaml")
			if err != nil {
				t.Fatal(err)
			}
			metricDocs, dashboardDocs, err := metrics.EncodePack(defs, []metrics.Dashboard{d}, dts)
			if err != nil {
				t.Fatal(err)
			}
			const siteName = "dashboards.example.test"
			snap := &doctype.ConfigSnapshot{DocTypes: dts, Metrics: metricDocs, Dashboards: dashboardDocs}
			if err = site.ImportConfigFromSnapshotForTenant(fixture.DB, doctype.NewRegistry(), fixture.Name, siteName, siteName, "dashboards round trip", snap, dialect, nil); err != nil {
				t.Fatal(err)
			}
			active, _, err := configstore.NewStore(fixture.DB, dialect).LoadActiveSnapshot(siteName)
			if err != nil {
				t.Fatal(err)
			}
			if len(active.Dashboards) != 1 {
				t.Fatalf("active config carries %d dashboards; want 1", len(active.Dashboards))
			}
			var back metrics.Dashboard
			if err := json.Unmarshal(active.Dashboards[0], &back); err != nil || !reflect.DeepEqual(back, d) {
				t.Fatalf("stored dashboard = %+v, %v; want %+v", back, err, d)
			}
		})
	}
}
