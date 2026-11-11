package analytics

import (
	"path/filepath"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

// The reports the Stock and Buying packs ship must use only models, measures and
// dimensions the Engine offers for their own record types.
func TestERPLayerReportsOnlyUseWhatTheEngineOffers(t *testing.T) {
	for pack, want := range map[string]int{"erp-inventory": 1, "erp-purchasing": 1} {
		t.Run(pack, func(t *testing.T) {
			root := filepath.Join("..", "..", "config", pack)
			docTypes, err := doctype.ParseConfigTree(root)
			if err != nil {
				t.Fatalf("parse %s: %v", pack, err)
			}
			reports, err := ParseReportsDirectory(filepath.Join(root, "reports"))
			if err != nil || len(reports) != want {
				t.Fatalf("%s reports = %d (%v), want %d", pack, len(reports), err, want)
			}
			catalog := BuildSemanticCatalog(docTypes)
			for index := range reports {
				if err := reports[index].Validate(catalog); err != nil {
					t.Fatalf("%s: %v", reports[index].Name, err)
				}
			}
		})
	}
}
