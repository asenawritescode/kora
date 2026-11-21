package metrics

import (
	"testing"
)

// TestSettleRunningSeries checks the bucket values of a balance (opening plus movements), a
// cumulative (from zero) and a simple series (the bucket's own sum) for each group.
func TestSettleRunningSeries(t *testing.T) {
	p := plan{
		from: day(2026, 11, 18), to: day(2026, 11, 20), grain: GrainDay,
		series: []series{{kind: KindBalance}, {kind: KindCumulative}, {kind: KindSimple}},
	}
	starts := bucketStarts(p.from, p.to, p.grain)
	agg := aggregate{sums: map[bucketKey]bucketSums{
		{period: openingPeriod, group: "P1"}:     {dec("6"), dec("99"), dec("99")}, // only the balance reads the opening
		{period: day(2026, 11, 18), group: "P1"}: {dec("-3"), dec("-3"), dec("4")},
		{period: day(2026, 11, 20), group: "P1"}: {dec("5"), dec("2"), dec("1")},
		{period: day(2026, 11, 19), group: "P2"}: {dec("-2"), dec("-2"), dec("1")},
	}}
	tests := []struct {
		name  string
		group string
		want  [][]string
	}{
		{"balance starts from its opening, gaps carry it forward", "P1", [][]string{
			{"3", "-3", "4"},
			{"3", "-3", "0"},
			{"8", "-1", "1"},
		}},
		{"no opening: balance starts at zero", "P2", [][]string{
			{"0", "0", "0"},
			{"-2", "-2", "1"},
			{"-2", "-2", "0"},
		}},
		{"group without rows settles to zero", "P3", [][]string{
			{"0", "0", "0"},
			{"0", "0", "0"},
			{"0", "0", "0"},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := p.settle(agg, tc.group, starts)
			if len(got) != len(tc.want) {
				t.Fatalf("buckets = %d, want %d", len(got), len(tc.want))
			}
			for i := range got {
				for j := range got[i] {
					if !got[i][j].Equal(dec(tc.want[i][j])) {
						t.Errorf("bucket %d series %d = %s, want %s", i, j, got[i][j], tc.want[i][j])
					}
				}
			}
		})
	}
}
