package cli

import "testing"

func TestSiteProbePercentileUsesNearestRank(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := percentile(values, .50); got != 5 {
		t.Fatalf("p50=%v, want 5", got)
	}
	if got := percentile(values, .95); got != 10 {
		t.Fatalf("p95=%v, want 10", got)
	}
}
