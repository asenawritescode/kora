package api

import (
	"testing"

	"github.com/asenawritescode/kora/internal/facts"
)

func TestSummariseHistory(t *testing.T) {
	tests := []struct {
		name     string
		progress map[string]facts.Progress
		state    string
		fraction float64
	}{
		{"no models read", nil, "ready", 1},
		{"every model ready", map[string]facts.Progress{"Sale": {Fraction: 1}, "Expense": {Fraction: 1}}, "ready", 1},
		{"one model rebuilding", map[string]facts.Progress{"Sale": {Building: true, Fraction: 0.4}, "Expense": {Fraction: 1}}, "building", 0.4},
		{"least progress wins", map[string]facts.Progress{"Sale": {Building: true, Fraction: 0.7}, "Expense": {Building: true, Fraction: 0.2}}, "building", 0.2},
		{"queued job counts as building at zero", map[string]facts.Progress{"Sale": {Building: true}}, "building", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := summariseHistory(tc.progress)
			if got.State != tc.state || got.Progress != tc.fraction {
				t.Fatalf("summariseHistory = %+v, want state %q progress %v", got, tc.state, tc.fraction)
			}
		})
	}
}
