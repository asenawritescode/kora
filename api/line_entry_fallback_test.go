package api

import (
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestLineEntryLayoutFallsBackWithoutLookupAccess(t *testing.T) {
	ref := contract.PublishedOperationRef("p.catalog", "p.catalog", "item_search")
	le := &contract.LineEntryLayout{LookupQuery: ref}
	for name, tc := range map[string]struct {
		visible map[string]bool
		layout  string
	}{"actor can lookup": {map[string]bool{ref: true}, "line_entry"}, "actor cannot lookup": {map[string]bool{}, "form"}} {
		t.Run(name, func(t *testing.T) {
			c := contract.ApplicationCatalog{PublishedOperations: []contract.ApplicationCatalogOperation{{Presentation: contract.SurfacePresentation{Layout: "line_entry", LineEntry: le}}}}
			downgradeUnreachableLineEntry(&c, tc.visible)
			got := c.PublishedOperations[0].Presentation
			if got.Layout != tc.layout || (tc.layout == "form") != (got.LineEntry == nil) {
				t.Fatalf("got %+v", got)
			}
		})
	}
}
