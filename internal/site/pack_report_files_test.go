package site

import (
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

func TestParseConfigPackStillRejectsUnknownContentType(t *testing.T) {
	files := []PackFile{{Path: "charts/x.yaml", ContentType: "chart", Content: "name: x\n"}}
	if _, _, err := parseConfigPack("retail", files); err == nil {
		t.Fatal("unknown content type accepted")
	}
}
