package analytics

import (
	"path/filepath"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

// The Produce report must use only models, measures and dimensions the Engine
// offers for the pack's record types.
func TestProduceReportsOnlyUseWhatTheEngineOffers(t *testing.T) {
	root := filepath.Join("..", "..", "config")
	docTypes, err := doctype.ParseConfigTree(filepath.Join(root, "farm-produce"))
	if err != nil {
		t.Fatalf("parse farm-produce: %v", err)
	}
	// Product is the selling pack's record type, which deliveries link to.
	selling, err := doctype.ParseConfigTree(filepath.Join(root, "pos-base"))
	if err != nil {
		t.Fatalf("parse pos-base: %v", err)
	}
	catalog := BuildSemanticCatalog(append(docTypes, selling...))
	reports, err := ParseReportsDirectory(filepath.Join(root, "farm-produce", "reports"))
	if err != nil || len(reports) == 0 {
		t.Fatalf("produce reports: %d, %v", len(reports), err)
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
