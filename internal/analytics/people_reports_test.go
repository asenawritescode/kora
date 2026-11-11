package analytics

import (
	"path/filepath"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

// The People report must use only models, measures and dimensions the Engine
// offers for the pack's record types.
func TestPeopleReportsOnlyUseWhatTheEngineOffers(t *testing.T) {
	pack := filepath.Join("..", "..", "config", "people")
	docTypes, err := doctype.ParseConfigTree(pack)
	if err != nil {
		t.Fatalf("parse people: %v", err)
	}
	catalog := BuildSemanticCatalog(docTypes)
	reports, err := ParseReportsDirectory(filepath.Join(pack, "reports"))
	if err != nil || len(reports) == 0 {
		t.Fatalf("people reports: %d, %v", len(reports), err)
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
