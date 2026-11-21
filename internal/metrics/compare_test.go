package metrics

import (
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/tenanttime"
)

func day(y int, m time.Month, d int) tenanttime.Date {
	return tenanttime.Date{Year: y, Month: m, Day: d}
}

func TestPreviousRange(t *testing.T) {
	tests := []struct {
		name     string
		from, to tenanttime.Date
		grain    Grain
		wantFrom tenanttime.Date
		wantTo   tenanttime.Date
	}{
		{"days: same length, ending the day before", day(2026, 11, 18), day(2026, 11, 20), GrainDay, day(2026, 11, 15), day(2026, 11, 17)},
		{"one day", day(2026, 11, 18), day(2026, 11, 18), GrainDay, day(2026, 11, 17), day(2026, 11, 17)},
		{"aligned week", day(2026, 11, 16), day(2026, 11, 22), GrainWeek, day(2026, 11, 9), day(2026, 11, 15)},
		{"unaligned week: one bucket back from its first bucket", day(2026, 11, 18), day(2026, 11, 20), GrainWeek, day(2026, 11, 9), day(2026, 11, 15)},
		{"aligned month", day(2026, 11, 1), day(2026, 11, 30), GrainMonth, day(2026, 10, 1), day(2026, 10, 31)},
		{"two months back across a year end", day(2026, 10, 15), day(2026, 11, 20), GrainMonth, day(2026, 8, 1), day(2026, 9, 30)},
		{"crosses a year boundary", day(2027, 1, 2), day(2027, 1, 4), GrainDay, day(2026, 12, 30), day(2027, 1, 1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := plan{from: tc.from, to: tc.to, grain: tc.grain}
			from, to := p.previousRange()
			if from != tc.wantFrom || to != tc.wantTo {
				t.Fatalf("previousRange = %s..%s, want %s..%s", from, to, tc.wantFrom, tc.wantTo)
			}
			if got, want := len(bucketStarts(from, to, tc.grain)), len(bucketStarts(tc.from, tc.to, tc.grain)); got != want {
				t.Fatalf("previous buckets = %d, want %d", got, want)
			}
		})
	}
}

// sumsPlan is a plan whose first requested metric is a simple sum over one series.
func sumsPlan() plan {
	return plan{series: make([]series, 2), metrics: []output{{metric: "sales", kind: KindSimple, num: 0, den: -1}}, requested: []int{0}}
}

// groupAgg builds one bucket per (group, total) pair on a single day.
func groupAgg(totals map[string]string) aggregate {
	agg := aggregate{sums: map[bucketKey]bucketSums{}, groups: map[string][]string{}}
	d := day(2026, 11, 18)
	for key, total := range totals {
		agg.groups[key] = []string{key}
		agg.sums[bucketKey{period: d, group: key}] = bucketSums{dec(total), dec("0")}
	}
	return agg
}

func TestTopKeysRanksByFirstMetric(t *testing.T) {
	agg := groupAgg(map[string]string{"A": "300", "B": "50", "C": "40", "D": "50"})
	tests := []struct {
		name string
		n    int
		want map[string]bool
	}{
		{"top two keeps the largest", 2, map[string]bool{"A": true, "B": true}},
		{"ties go to the smaller key", 3, map[string]bool{"A": true, "B": true, "D": true}},
		{"n above the group count keeps all", 9, map[string]bool{"A": true, "B": true, "C": true, "D": true}},
		{"top one", 1, map[string]bool{"A": true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sumsPlan().topKeys(agg, tc.n)
			if len(got) != len(tc.want) {
				t.Fatalf("keep = %v, want %v", got, tc.want)
			}
			for k := range tc.want {
				if !got[k] {
					t.Fatalf("keep = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestFoldSumsTheRestIntoOther(t *testing.T) {
	agg := groupAgg(map[string]string{"A": "300", "B": "50", "C": "40"})
	folded := agg.fold(map[string]bool{"A": true})
	if got := folded.groups[otherGroup]; len(got) != 1 || got[0] != otherGroup {
		t.Fatalf("other group = %v", got)
	}
	other := folded.sums[bucketKey{period: day(2026, 11, 18), group: otherGroup}]
	if !other[0].Equal(dec("90")) {
		t.Fatalf("other sales = %s, want 90", other[0])
	}
	if _, ok := folded.sums[bucketKey{period: day(2026, 11, 18), group: "B"}]; ok {
		t.Fatalf("group B should fold away")
	}
}

func TestOtherRatioDividesSummedComponents(t *testing.T) {
	// Receipts: B has sales 50 over 1, C has 40 over 4. Averaging the ratios gives 30; the sums give 18.
	agg := aggregate{sums: map[bucketKey]bucketSums{
		{period: day(2026, 11, 18), group: "A"}: {dec("300"), dec("3")},
		{period: day(2026, 11, 18), group: "B"}: {dec("50"), dec("1")},
		{period: day(2026, 11, 18), group: "C"}: {dec("40"), dec("4")},
	}, groups: map[string][]string{"A": {"A"}, "B": {"B"}, "C": {"C"}}}
	p := plan{series: make([]series, 2), metrics: []output{{metric: "avg", kind: KindRatio, num: 0, den: 1}}, requested: []int{0}}
	folded := agg.fold(map[string]bool{"A": true})
	got := p.values(folded.sums[bucketKey{period: day(2026, 11, 18), group: otherGroup}])[0]
	if !got.Valid || !got.Decimal.Equal(dec("18")) {
		t.Fatalf("other average = %+v, want 18", got)
	}
}

func TestDerivedComputesComponentsFirst(t *testing.T) {
	cat := posCatalog(t)
	q := Query{Metrics: []string{"net_sales", "average_plus_one"}, From: day(2026, 11, 18), To: day(2026, 11, 20), Grain: GrainDay}
	p, err := planQuery(cat, q)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	if len(p.series) != 3 {
		t.Fatalf("series = %d, want 3 (sales, refunds, receipts) shared between metrics", len(p.series))
	}
	names := p.metricNames()
	if names[0] != "net_sales" || names[1] != "average_plus_one" {
		t.Fatalf("metric names = %v", names)
	}
	for i, o := range p.metrics {
		if o.kind == KindDerived {
			for _, leaf := range []*formulaNode{o.expr} {
				if !boundBefore(leaf, i) {
					t.Fatalf("metric %q reads a slot at or after itself", o.metric)
				}
			}
		}
	}
}

// boundBefore reports whether every metric reference in n reads a slot below limit.
func boundBefore(n *formulaNode, limit int) bool {
	if n.op == 0 {
		return n.ref == "" || n.slot < limit
	}
	return boundBefore(n.left, limit) && boundBefore(n.right, limit)
}
