package metrics

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/internal/tenanttime"
)

// otherGroup is the group that holds every group outside the top N. Its label is otherLabel.
const (
	otherGroup = "__other__"
	otherLabel = "Other"
)

// previousRange is the window of as many buckets as the range has, ending the day before its
// first bucket. For a range that starts on a bucket boundary that is the day before From. A range
// grain takes as many days as the range has, ending the day before From. A week_before window is
// the range one week earlier.
func (p plan) previousRange() (tenanttime.Date, tenanttime.Date) {
	if p.compare == CompareWeekBefore {
		shifted := Span{p.from, p.to}.weekBefore()
		return shifted.From, shifted.To
	}
	if p.grain == GrainRange {
		start := toTime(p.from)
		days := int(toTime(p.to).Sub(start).Hours()/24) + 1
		return toDate(start.AddDate(0, 0, -days)), toDate(start.AddDate(0, 0, -1))
	}
	first := periodStart(toTime(p.from), p.grain)
	n := len(bucketStarts(p.from, p.to, p.grain))
	return toDate(stepPeriod(first, p.grain, -n)), toDate(first.AddDate(0, 0, -1))
}

// compares reports whether the plan reads a comparison window.
func (p plan) compares() bool {
	return p.compare == ComparePreviousPeriod || p.compare == CompareWeekBefore
}

// previousRows runs the previous-period window and returns its rows aligned with cur's rows. It
// uses the current keys: with top-N, the previous Other group is every group outside the current
// top N. A group that appears only in the previous period has no current row and is dropped.
func (e Engine) previousRows(ctx context.Context, p plan, cur aggregate, keys []string, labels []map[string]string) ([]Row, error) {
	from, to := p.previousRange()
	prevPlan := p
	prevPlan.from, prevPlan.to = from, to
	prev, err := e.run(ctx, prevPlan)
	if err != nil {
		return nil, err
	}
	if p.top > 0 {
		prev = prev.fold(keptGroups(keys))
	}
	prev.groups = cur.groups
	return p.rows(prev, keys, from, to, labels), nil
}

// keptGroups is the set of keys that are real groups: every key except the Other group.
func keptGroups(keys []string) map[string]bool {
	kept := make(map[string]bool, len(keys))
	for _, k := range keys {
		if k != otherGroup {
			kept[k] = true
		}
	}
	return kept
}

// fold keeps the groups in keep and sums every other group into the Other group, bucket by bucket.
// Sums of components fold exactly, so a ratio computed afterwards divides summed components.
func (agg aggregate) fold(keep map[string]bool) aggregate {
	out := aggregate{sums: map[bucketKey]bucketSums{}, groups: map[string][]string{}}
	rest := false
	for bk, vals := range agg.sums {
		if keep[bk.group] {
			out.sums[bk] = vals
			out.groups[bk.group] = agg.groups[bk.group]
			continue
		}
		rest = true
		other := bucketKey{period: bk.period, group: otherGroup}
		out.sums[other] = out.sums[other].add(vals)
	}
	if rest {
		out.groups[otherGroup] = []string{otherGroup}
	}
	return out
}

// rankedGroup is one group with its value for the ranking metric over the whole range.
type rankedGroup struct {
	key   string
	value decimal.NullDecimal
}

// compare orders groups for the top N: larger values first, null last, then the smaller key.
func (g rankedGroup) compare(h rankedGroup) int {
	if g.value.Valid != h.value.Valid {
		if g.value.Valid {
			return -1
		}
		return 1
	}
	if g.value.Valid {
		if c := h.value.Decimal.Cmp(g.value.Decimal); c != 0 {
			return c
		}
	}
	return strings.Compare(g.key, h.key)
}

// topKeys ranks the groups by the first requested metric and keeps the first n. A metric that
// depends on a running series ranks by its value in the last bucket; any other sums its buckets.
func (p plan) topKeys(agg aggregate, n int) map[string]bool {
	starts := bucketStarts(p.from, p.to, p.grain)
	ranked := make([]rankedGroup, 0, len(agg.groups))
	for key := range agg.groups {
		ranked = append(ranked, rankedGroup{key: key, value: p.rankValue(agg, key, starts)})
	}
	slices.SortFunc(ranked, rankedGroup.compare)
	keep := make(map[string]bool, n)
	for _, r := range ranked[:min(n, len(ranked))] {
		keep[r.key] = true
	}
	return keep
}

// rankValue is the first requested metric of one group over the range: its last bucket when the
// metric is running, otherwise the sum of its buckets. The opening bucket is never summed.
func (p plan) rankValue(agg aggregate, key string, starts []time.Time) decimal.NullDecimal {
	if p.metrics[p.requested[0]].running {
		settled := p.settle(agg, key, starts)
		return p.values(settled[len(settled)-1])[0]
	}
	total := make(bucketSums, len(p.series))
	for _, start := range starts {
		total = total.add(agg.sums[bucketKey{period: toDate(start), group: key}])
	}
	return p.values(total)[0]
}
