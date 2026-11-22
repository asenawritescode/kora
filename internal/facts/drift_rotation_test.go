package facts

import (
	"fmt"
	"slices"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

// TestDriftRunModelsIsBoundedAndCoversEveryModel checks the drift check's model selection: declared models
// are checked every run, generated models rotate through the remaining slots, and over enough runs every
// generated model is checked.
func TestDriftRunModelsIsBoundedAndCoversEveryModel(t *testing.T) {
	tests := []struct {
		name            string
		declared, gener int
		wantPerRun      int
		runs            int
		// declaredOnly is set when declared models alone fill the bound: then they rotate among themselves
		// and the generated models are not checked.
		declaredOnly bool
	}{
		{name: "everything fits in one run", declared: 2, gener: 3, wantPerRun: 5, runs: 1},
		{name: "generated models rotate past the bound", declared: 2, gener: 12, wantPerRun: driftModelsPerRun, runs: 2},
		{name: "many declared models rotate among themselves", declared: 10, gener: 4, wantPerRun: driftModelsPerRun, runs: 2, declaredOnly: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := doctype.NewRegistry()
			for i := 0; i < tc.declared; i++ {
				reg.Register(&doctype.DocType{Name: fmt.Sprintf("Declared%02d", i), Facts: &doctype.FactModel{Time: "creation", Measures: []doctype.FactMeasure{{Field: "total"}}}})
			}
			for i := 0; i < tc.gener; i++ {
				reg.Register(&doctype.DocType{Name: fmt.Sprintf("Generated%02d", i), Fields: []doctype.Field{{Fieldname: "amount", Fieldtype: "Currency"}}})
			}
			c := &DriftChecker{Registry: reg}
			seen := map[string]bool{}
			for run := 0; run < tc.runs; run++ {
				picked := c.runModels()
				if len(picked) != tc.wantPerRun {
					t.Fatalf("run %d checked %d models, want %d", run, len(picked), tc.wantPerRun)
				}
				for _, dt := range picked {
					seen[dt.Name] = true
				}
			}
			for _, dt := range reg.All() {
				if tc.declaredOnly && dt.Facts.Generated {
					continue
				}
				if !seen[dt.Name] {
					t.Errorf("%s was never checked in %d runs", dt.Name, tc.runs)
				}
			}
		})
	}
}

// TestDriftDeclaredModelsAreCheckedEveryRun checks that the declared models lead every run while they fit.
func TestDriftDeclaredModelsAreCheckedEveryRun(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Sale", Facts: &doctype.FactModel{Time: "creation", Measures: []doctype.FactMeasure{{Field: "total"}}}})
	for i := 0; i < 12; i++ {
		reg.Register(&doctype.DocType{Name: fmt.Sprintf("Generated%02d", i), Fields: []doctype.Field{{Fieldname: "amount", Fieldtype: "Currency"}}})
	}
	c := &DriftChecker{Registry: reg}
	for run := 0; run < 3; run++ {
		names := []string{}
		for _, dt := range c.runModels() {
			names = append(names, dt.Name)
		}
		if !slices.Contains(names, "Sale") {
			t.Fatalf("run %d did not check the declared Sale: %v", run, names)
		}
	}
}
