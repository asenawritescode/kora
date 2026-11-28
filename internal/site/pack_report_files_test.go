package site

import (
	"github.com/asenawritescode/kora/contract"
	"path/filepath"
	"testing"
)

// A release published before the reports/ removal still ships report files next to its real files.
// Those report files must not fail the install, and the pack's metrics must still load.
func TestPackPartIgnoresRetiredReportFiles(t *testing.T) {
	base := readPackFiles(t, filepath.Join("..", "..", "config", "pos"), "")
	cases := []struct {
		name    string
		reports []PackFile
	}{
		{"one report", []PackFile{
			{Path: "reports/profit.yaml", ContentType: "report", Content: "name: profit\n"},
		}},
		{"two reports", []PackFile{
			{Path: "reports/profit.yaml", ContentType: "report", Content: "name: profit\n"},
			{Path: "reports/stock.yml", ContentType: "report", Content: "name: stock\n"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := append(append([]PackFile{}, base...), tc.reports...)
			snap, _, err := parsePackPart("pos", files)
			if err != nil {
				t.Fatalf("parsePackPart with retired report files: %v", err)
			}
			if len(snap.Metrics) != posMetricCount {
				t.Fatalf("metrics %d; want %d", len(snap.Metrics), posMetricCount)
			}
		})
	}
}

// The Engine validates a composed release before Cloud accepts it. Report files from a
// published module must validate with no findings, next to the real POS files.
func TestValidatePackAcceptsRetiredReportFiles(t *testing.T) {
	files := readPackFiles(t, filepath.Join("..", "..", "config", "pos"), "")
	files = append(files, PackFile{Path: "reports/components/633d951f29be55aa/sales_overview.yaml", ContentType: "report", Content: "name: sales_overview\n"})
	v, err := ValidatePack(PackManifest{SchemaVersion: 1, Name: "pos", Version: "1.0.0", Requires: []contract.ManifestPackRef{{PackageID: "inventory", Version: "1"}}}, files)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Valid {
		t.Fatalf("report files rejected: %+v", v.Findings)
	}
}

func TestParseConfigPackStillRejectsUnknownContentType(t *testing.T) {
	files := []PackFile{{Path: "charts/x.yaml", ContentType: "chart", Content: "name: x\n"}}
	if _, _, err := parseConfigPack("retail", files); err == nil {
		t.Fatal("unknown content type accepted")
	}
}
