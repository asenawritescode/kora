package metrics

import (
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// Aggregates a series can compute.
const (
	aggSum           = "sum"
	aggCount         = "count"
	aggCountDistinct = "count_distinct"
)

// plan is a compiled query: the source to read, the metrics to compute and the grouping.
// Planning does no I/O; the SQL builder turns a plan into statements.
type plan struct {
	model     *doctype.DocType
	summary   bool // read _kora_fact_summary_<model>; otherwise read the fact tables
	grain     Grain
	from      tenanttime.Date
	to        tenanttime.Date
	dims      []dimension // dimensions the result is grouped by, in request order
	series    []series    // one aggregate per simple metric, shared by every metric that uses it
	metrics   []output    // every metric the query computes; a metric follows the metrics it reads
	requested []int       // index into metrics of each requested metric, in request order
	where     []predicate // query filters, applied to every series
	scope     Scope
	top       int
	compare   Compare
	opening   bool // read only the days before from: the opening of every balance series
}

// series is one aggregate of one metric. Its own filters apply only to that series.
type series struct {
	metric string
	kind   Kind // simple, balance or cumulative: the bucket sums are settled per kind
	agg    string
	column string // measure column for sum, dimension for count_distinct, empty for count
	conds  []predicate
}

// running reports whether the series accumulates over the buckets, see Kind.running.
func (s series) running() bool { return s.kind.running() }

// output is one computed metric. A simple, balance or cumulative metric reads series num; a ratio
// divides series num by series den; a derived metric evaluates expr over the values of the metrics it
// names. running is set when the value depends on a running series, so ranking reads the last bucket.
type output struct {
	metric  string
	kind    Kind
	num     int
	den     int
	expr    *formulaNode
	running bool
}

// predicate compares a column with a value. For status the field is the status field name; for a
// dimension it is the dimension column name.
type predicate struct {
	field string
	op    string
	value any
}

// planner compiles requested metrics into one plan. Metrics and series named by several requests
// are compiled once.
type planner struct {
	cat     Catalog
	plan    plan
	series  map[string]int // simple metric name -> index in plan.series
	metrics map[string]int // metric name -> index in plan.metrics; -1 while it is being compiled
}

// planQuery checks the query against the catalog and compiles it into a plan.
func planQuery(cat Catalog, q Query) (plan, error) {
	if err := checkShape(q); err != nil {
		return plan{}, err
	}
	c := &planner{
		cat:     cat,
		plan:    plan{grain: q.Grain, from: q.From, to: q.To, scope: q.Scope, top: q.Top, compare: q.Compare},
		series:  map[string]int{},
		metrics: map[string]int{},
	}
	for _, name := range q.Metrics {
		i, err := c.compile(name)
		if err != nil {
			return plan{}, err
		}
		c.plan.requested = append(c.plan.requested, i)
	}
	if err := c.plan.addGrouping(cat, q); err != nil {
		return plan{}, err
	}
	c.plan.summary = c.plan.fitsSummary()
	return c.plan, nil
}

// checkShape validates the parts of a query that do not depend on the catalog.
func checkShape(q Query) error {
	switch {
	case len(q.Metrics) == 0:
		return errors.New("a query names at least one metric")
	case q.Grain != GrainDay && q.Grain != GrainWeek && q.Grain != GrainMonth && q.Grain != GrainRange:
		return fmt.Errorf("grain %q must be day, week, month or range", q.Grain)
	case toTime(q.To).Before(toTime(q.From)):
		return fmt.Errorf("range ends %s before it starts %s", q.To, q.From)
	case q.Compare != "" && q.Compare != CompareNone && q.Compare != ComparePreviousPeriod:
		return fmt.Errorf("compare %q must be none or previous_period", q.Compare)
	case q.Top < 0:
		return fmt.Errorf("top %d must not be negative", q.Top)
	case q.Top > 0 && len(q.Dimensions) != 1:
		return errors.New("top needs exactly one dimension")
	case rangeTooLarge(q):
		return fmt.Errorf("%w: a %s query covers at most %d buckets", ErrRangeTooLarge, q.Grain, maxBuckets[q.Grain])
	}
	return nil
}

// maxBuckets caps the buckets one query may return, per grain.
var maxBuckets = map[Grain]int{GrainDay: 366, GrainWeek: 157, GrainMonth: 120}

// rangeTooLarge reports whether q covers more buckets than maxBuckets allows. It stops counting
// one bucket past the cap, so a huge range costs no more than the cap. A range grain is one bucket
// and never exceeds the cap.
func rangeTooLarge(q Query) bool {
	if q.Grain == GrainRange {
		return false
	}
	limit := maxBuckets[q.Grain]
	n := 0
	for b := periodStart(toTime(q.From), q.Grain); !b.After(toTime(q.To)); b = stepPeriod(b, q.Grain, 1) {
		if n++; n > limit {
			return true
		}
	}
	return false
}

// compile returns the index in plan.metrics of the named metric, compiling it and the metrics it
// reads first.
func (c *planner) compile(name string) (int, error) {
	if i, ok := c.metrics[name]; ok {
		if i < 0 {
			return 0, fmt.Errorf("metric %q is part of a cycle", name)
		}
		return i, nil
	}
	m, ok := c.cat.Metrics[name]
	if !ok {
		return 0, fmt.Errorf("unknown metric %q", name)
	}
	c.metrics[name] = -1
	out, err := c.output(m)
	if err != nil {
		return 0, err
	}
	c.plan.metrics = append(c.plan.metrics, out)
	c.metrics[name] = len(c.plan.metrics) - 1
	return c.metrics[name], nil
}

// output compiles one metric's own computation.
func (c *planner) output(m Metric) (output, error) {
	switch m.Type {
	case KindSimple:
		num, err := c.addSeries(m)
		if err != nil {
			return output{}, err
		}
		return output{metric: m.Name, kind: KindSimple, num: num, den: -1}, nil
	case KindRatio:
		num, err := c.componentSeries(m, m.Numerator)
		if err != nil {
			return output{}, err
		}
		den, err := c.componentSeries(m, m.Denominator)
		if err != nil {
			return output{}, err
		}
		return output{metric: m.Name, kind: KindRatio, num: num, den: den}, nil
	case KindDerived:
		return c.derived(m)
	case KindBalance, KindCumulative:
		num, err := c.addSeries(m)
		if err != nil {
			return output{}, err
		}
		return output{metric: m.Name, kind: m.Type, num: num, den: -1, running: true}, nil
	}
	return output{}, fmt.Errorf("metric %q has unknown type %q", m.Name, m.Type)
}

// derived parses a formula, compiles every metric it names and binds each name to that metric's index.
func (c *planner) derived(m Metric) (output, error) {
	root, err := parseFormula(m.Formula)
	if err != nil {
		return output{}, fmt.Errorf("metric %q: %w", m.Name, err)
	}
	slots := map[string]int{}
	running := false
	for _, ref := range m.refs() {
		i, err := c.compile(ref)
		if err != nil {
			return output{}, fmt.Errorf("metric %q: %w", m.Name, err)
		}
		slots[ref] = i
		running = running || c.plan.metrics[i].running
	}
	root.bind(slots)
	return output{metric: m.Name, kind: KindDerived, expr: root, running: running}, nil
}

// componentSeries resolves a ratio's numerator or denominator, which must be a simple metric.
func (c *planner) componentSeries(ratio Metric, name string) (int, error) {
	m, ok := c.cat.Metrics[name]
	if !ok {
		return 0, fmt.Errorf("metric %q refers to unknown metric %q", ratio.Name, name)
	}
	if m.Type.running() {
		return 0, fmt.Errorf("ratio %q divides by %s metric %q: ratio components must be simple", ratio.Name, m.Type, name)
	}
	if m.Type != KindSimple {
		return 0, fmt.Errorf("ratio %q divides by %s metric %q: %w", ratio.Name, m.Type, name, ErrNotSupported)
	}
	return c.addSeries(m)
}

// addSeries adds the aggregate of a simple metric once and returns its index.
func (c *planner) addSeries(m Metric) (int, error) {
	if i, ok := c.series[m.Name]; ok {
		return i, nil
	}
	dt := c.cat.Models[m.Model]
	if dt == nil || dt.Facts == nil {
		return 0, fmt.Errorf("metric %q: model %q is not a record type with facts", m.Name, m.Model)
	}
	if m.Measure == nil {
		return 0, fmt.Errorf("metric %q has no measure", m.Name)
	}
	if c.plan.model != nil && c.plan.model != dt {
		return 0, fmt.Errorf("metric %q reads %q, not %q: metrics of one query must share a model: %w", m.Name, m.Model, c.plan.model.Name, ErrNotSupported)
	}
	if err := m.checkAdditive(); err != nil {
		return 0, fmt.Errorf("metric %q: %w", m.Name, err)
	}
	s := series{metric: m.Name, kind: m.Type, agg: m.Measure.Agg}
	switch m.Measure.Agg {
	case aggSum, aggCountDistinct:
		s.column = m.Measure.Field
	case aggCount:
	default:
		return 0, fmt.Errorf("metric %q: agg %q is not sum, count or count_distinct", m.Name, m.Measure.Agg)
	}
	for _, f := range m.Filters {
		if err := f.validate(dt.Facts); err != nil {
			return 0, fmt.Errorf("metric %q: %w", m.Name, err)
		}
		s.conds = append(s.conds, predicate{field: f.Field, op: f.Op, value: f.Value})
	}
	c.plan.model = dt
	c.plan.series = append(c.plan.series, s)
	c.series[m.Name] = len(c.plan.series) - 1
	return c.series[m.Name], nil
}

// addGrouping resolves the dimensions and checks the query filters against the model.
func (p *plan) addGrouping(cat Catalog, q Query) error {
	if p.model == nil {
		return errors.New("no metric reads a record type")
	}
	for _, name := range q.Dimensions {
		dim, err := p.dimensionOf(cat, name, len(p.dims))
		if err != nil {
			return err
		}
		p.dims = append(p.dims, dim)
	}
	for _, f := range q.Filters {
		if err := f.validate(p.model.Facts); err != nil {
			return err
		}
		p.where = append(p.where, predicate{field: f.Field, op: f.Op, value: f.Value})
	}
	return nil
}

// fitsSummary reports whether the summary table can answer the plan. Summaries are per day, per
// status, and per one declared dimension, and they carry no owner. Distinct counts and dimension paths
// need the facts.
func (p plan) fitsSummary() bool {
	if p.scope.Owner != "" || len(p.dims) > 1 || p.hasJoins() {
		return false
	}
	usable := func(field string) bool {
		return field == p.model.Facts.StatusField || (len(p.dims) == 1 && field == p.dims[0].name)
	}
	for _, s := range p.series {
		if s.agg == aggCountDistinct {
			return false
		}
		for _, c := range s.conds {
			if !usable(c.field) {
				return false
			}
		}
	}
	for _, c := range p.where {
		if !usable(c.field) {
			return false
		}
	}
	return true
}

// needsOpening reports whether a balance reads the days before the range as well.
func (p plan) needsOpening() bool {
	for _, s := range p.series {
		if s.kind == KindBalance {
			return true
		}
	}
	return false
}

// metricNames lists the requested metrics by name, in request order.
func (p plan) metricNames() []string {
	names := make([]string, len(p.requested))
	for i, r := range p.requested {
		names[i] = p.metrics[r].metric
	}
	return names
}
