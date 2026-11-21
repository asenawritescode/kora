package analytics

import (
	"path/filepath"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

// Every report in config/erpsell must use only models, measures and
// dimensions the Engine really offers for the pack's record types, so a rename
// of a field cannot leave a report quietly empty.
func TestPOSBaseReportsOnlyUseWhatTheEngineOffers(t *testing.T) {
	skipParkedPacks(t)
	pack := filepath.Join("..", "..", "config", "erpsell")
	docTypes, err := doctype.ParseConfigTree(pack)
	if err != nil {
		t.Fatalf("parse erpsell: %v", err)
	}
	catalog := BuildSemanticCatalog(docTypes)
	reports, err := ParseReportsDirectory(filepath.Join(pack, "reports"))
	if err != nil {
		t.Fatalf("parse erpsell reports: %v", err)
	}
	if len(reports) < 3 {
		t.Fatalf("expected the sales, payments and expenses reports, found %d", len(reports))
	}
	for index := range reports {
		report := reports[index]
		t.Run(report.Name, func(t *testing.T) {
			if err := report.Validate(catalog); err != nil {
				t.Fatal(err)
			}
		})
	}
}
