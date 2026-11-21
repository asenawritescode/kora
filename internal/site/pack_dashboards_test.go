package site

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/metrics"
)

// testDashboardYAML is a test-only dashboard over the POS sales metric.
const testDashboardYAML = "name: till_insights\nlabel: Till insights\ndefault_range: last_30_days\ncharts:\n  - id: sales_by_day\n    title: Sales by day\n    kind: bar\n    metrics: [sales]\n    by: time\n    grain: day\n"

// posPackWithDashboard is the POS pack plus one test-only dashboard file.
func posPackWithDashboard(t *testing.T) []PackFile {
	t.Helper()
	// The shipped pos_insights dashboard is left out: this fixture pins exactly one dashboard.
	var files []PackFile
	for _, f := range readPackFiles(t, filepath.Join("..", "..", "config", "pos"), "") {
		if !strings.HasPrefix(f.Path, "dashboards/") {
			files = append(files, f)
		}
	}
	return append(files, PackFile{Path: "dashboards/till_insights.yaml", ContentType: "dashboard", Content: testDashboardYAML})
}

// posRecordTypes are the record types the POS metrics and dashboards are checked against.
func posRecordTypes(t *testing.T) []*doctype.DocType {
	t.Helper()
	var dts []*doctype.DocType
	for _, pack := range []string{"inventory", "pos"} {
		parsed, err := doctype.ParseConfigTree(filepath.Join("..", "..", "config", pack))
		if err != nil {
			t.Fatal(err)
		}
		dts = append(dts, parsed...)
	}
	return dts
}

// The directory, package and upload install paths each carry the same dashboard definitions.
func TestDashboardDefinitionsAgreeAcrossInstallPaths(t *testing.T) {
	dts := posRecordTypes(t)

	// Directory: the POS metrics and a dashboards/ folder beside them.
	packDir := t.TempDir()
	copyPOSMetrics(t, packDir)
	manifest, err := os.ReadFile(filepath.Join("..", "..", "config", "pos", "pack.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(packDir, "pack.yaml"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(packDir, "dashboards"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "dashboards", "till_insights.yaml"), []byte(testDashboardYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, directory, err := LoadPackReportsAndMetrics(packDir, dts)
	if err != nil {
		t.Fatal(err)
	}

	files := posPackWithDashboard(t)
	pkg, _, err := ParseConfigPack("pos", files)
	if err != nil {
		t.Fatal(err)
	}
	upload, _, err := parsePackPart("pos", files)
	if err != nil {
		t.Fatal(err)
	}

	want := decodeDashboards(t, directory)
	if len(want) != 1 || want[0].Name != "till_insights" {
		t.Fatalf("directory install carried dashboards %+v; want till_insights", want)
	}
	for name, raw := range map[string][]json.RawMessage{"package": pkg.Dashboards, "upload": upload.Dashboards} {
		if got := decodeDashboards(t, raw); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s install dashboards = %+v; want %+v", name, got, want)
		}
	}
}

func TestUploadedPackRejectsBadDashboardFile(t *testing.T) {
	files := posPackWithDashboard(t)
	files[len(files)-1].Content = strings.Replace(testDashboardYAML, "kind: bar", "kind: pie", 1)
	if _, _, err := parsePackPart("pos", files); err == nil || !strings.Contains(err.Error(), `kind "pie"`) {
		t.Fatalf("upload with bad dashboard = %v; want kind error", err)
	}
}

func TestPackSyntaxCheckRejectsUnknownDashboardKey(t *testing.T) {
	err := validatePackFileSyntax(PackFile{Path: "dashboards/x.yaml", ContentType: "dashboard", Content: "name: x\nlable: X\n"})
	if err == nil || !strings.Contains(err.Error(), `did you mean "label"`) {
		t.Fatalf("syntax check = %v; want did-you-mean for label", err)
	}
}

// copyPOSMetrics copies the POS metric documents into dir/metrics.
func copyPOSMetrics(t *testing.T, dir string) {
	t.Helper()
	src := filepath.Join("..", "..", "config", "pos", "metrics")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "metrics"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		body, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "metrics", e.Name()), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// decodeDashboards turns the snapshot documents back into dashboards, sorted by name.
func decodeDashboards(t *testing.T, raw []json.RawMessage) []metrics.Dashboard {
	t.Helper()
	out := make([]metrics.Dashboard, 0, len(raw))
	for _, r := range raw {
		var d metrics.Dashboard
		if err := json.Unmarshal(r, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}
