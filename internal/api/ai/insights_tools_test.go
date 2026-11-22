package ai

import (
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func TestWithoutMetricsToolsDropsOnlyMetricsReads(t *testing.T) {
	all := buildOpenAIToolsFromCatalog(BuildToolCatalog(doctype.NewRegistry()))
	tests := []struct {
		name        string
		insights    bool
		wantMetrics bool
	}{
		{name: "cashier without insights gets no metrics tools", insights: false, wantMetrics: false},
		{name: "manager with insights keeps them", insights: true, wantMetrics: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			functions := all
			if !tc.insights {
				functions = withoutMetricsTools(all)
			}
			names := map[string]bool{}
			for _, fn := range functions {
				names[safeGetString(safeGetMap(fn, "function"), "name")] = true
			}
			for _, name := range []string{toolListMetrics, toolQueryMetrics, toolDrillMetrics} {
				if names[name] != tc.wantMetrics {
					t.Fatalf("tool %s offered = %v, want %v", name, names[name], tc.wantMetrics)
				}
			}
			if !names[toolAskChoice] {
				t.Fatal("ask_choice was dropped; only the metrics reads go")
			}
			if len(functions) == 0 || (!tc.insights && len(functions) >= len(all)) {
				t.Fatalf("tool count = %d of %d", len(functions), len(all))
			}
		})
	}
	if !strings.Contains(insightsUnavailableNote, "Insights is not available") {
		t.Fatal("note does not tell the model Insights is unavailable")
	}
}
