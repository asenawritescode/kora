package site

import "testing"

// A release published before the reports/ removal still ships report files.
// They must install without error and must not block the metric and dashboard files.
func TestParseConfigPackIgnoresRetiredReportFiles(t *testing.T) {
	cases := []struct {
		name  string
		files []PackFile
	}{
		{"report only", []PackFile{
			{Path: "reports/profit.yaml", ContentType: "report", Content: "name: profit\n"},
		}},
		{"report with another report", []PackFile{
			{Path: "reports/profit.yaml", ContentType: "report", Content: "name: profit\n"},
			{Path: "reports/stock.yml", ContentType: "report", Content: "name: stock\n"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := parseConfigPack("retail", tc.files); err != nil {
				t.Fatalf("parseConfigPack with retired report files: %v", err)
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
