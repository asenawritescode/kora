package site

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/metrics"
)

// reportMetricPacks are the packs whose reports/ folder was converted to metrics/ and dashboards/
// (AN-E9b). The parked ones are read from config/_parked directly: skipParkedPacks would skip them.
var reportMetricPacks = []string{
	"appointments", "assets", "crm", "documents", "fleet", "grants-donors", "members-savings",
	"people", "projects", "properties", "rooms-bookings", "shipments", "students", "subscriptions",
	"_parked/batch-expiry", "_parked/farm-produce", "_parked/job-cards", "_parked/manufacturing",
	"_parked/menu-tables",
	"_parked/erpsell",
}

// packFilesAt reads a pack's doctype, metric and dashboard files from its directory under config/.
func packFilesAt(t *testing.T, pack string) []PackFile {
	t.Helper()
	root := filepath.Join("..", "..", "config", pack)
	var files []PackFile
	for dir, contentType := range map[string]string{"doctypes": "doctype", "metrics": "metric", "dashboards": "dashboard"} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) != ".yaml" {
				continue
			}
			body, err := os.ReadFile(filepath.Join(root, dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, PackFile{Path: dir + "/" + entry.Name(), ContentType: contentType, Content: string(body)})
		}
	}
	for name, contentType := range map[string]string{"roles.yaml": "roles", "permissions.yaml": "permissions"} {
		if body, err := os.ReadFile(filepath.Join(root, name)); err == nil {
			files = append(files, PackFile{Path: name, ContentType: contentType, Content: string(body)})
		}
	}
	return files
}

// Each converted pack validates on its own, and every metric and dashboard file it ships is present.
func TestReportMetricPacksValidate(t *testing.T) {
	for _, pack := range reportMetricPacks {
		t.Run(strings.ReplaceAll(pack, "/", "_"), func(t *testing.T) {
			files := packFilesAt(t, pack)
			metricCount, dashboardCount := 0, 0
			for _, f := range files {
				switch f.ContentType {
				case "metric":
					metricCount++
				case "dashboard":
					dashboardCount++
				}
			}
			// erpsell keeps its two selling dashboards, so a pack may ship more than one.
			if metricCount == 0 || dashboardCount == 0 {
				t.Fatalf("%s: got %d metrics and %d dashboards, want at least one metric and one dashboard", pack, metricCount, dashboardCount)
			}
			requireValidPack(t, filepath.Base(pack), files)
			checkPackMetricsAndDashboards(t, pack, metricCount, dashboardCount)
		})
	}
}

// checkPackMetricsAndDashboards runs the import-time checks the marketplace validation skips: metric and
// dashboard fields are checked against the pack's record types, facts and generated facts included.
func checkPackMetricsAndDashboards(t *testing.T, pack string, metricCount, dashboardCount int) {
	t.Helper()
	dts, err := doctype.ParseConfigTree(filepath.Join("..", "..", "config", pack))
	if err != nil {
		t.Fatal(err)
	}
	var defs []metrics.Metric
	var dashes []metrics.Dashboard
	for _, dir := range []string{"metrics", "dashboards"} {
		entries, readErr := os.ReadDir(filepath.Join("..", "..", "config", pack, dir))
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, entry := range entries {
			body, readErr := os.ReadFile(filepath.Join("..", "..", "config", pack, dir, entry.Name()))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if dir == "metrics" {
				m, parseErr := metrics.ParseYAML(body, entry.Name())
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				defs = append(defs, m)
				continue
			}
			d, parseErr := metrics.ParseDashboardYAML(body, entry.Name())
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			dashes = append(dashes, d)
		}
	}
	if len(defs) != metricCount || len(dashes) != dashboardCount {
		t.Fatalf("%s: parsed %d metrics and %d dashboards, want %d and %d", pack, len(defs), len(dashes), metricCount, dashboardCount)
	}
	reg := doctype.NewRegistry()
	for _, dt := range dts {
		reg.Register(dt) // Register gives each record type without a declared fact model its generated one.
	}
	if _, _, err := metrics.EncodePack(defs, dashes, reg.All()); err != nil {
		t.Fatalf("%s: metrics and dashboards do not validate: %v", pack, err)
	}
}
