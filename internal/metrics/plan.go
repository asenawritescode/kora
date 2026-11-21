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

// plan is a compiled query: the source to read, the aggregates to compute and the grouping.
// Planning does no I/O; the SQL builder turns a plan into statements.
type plan struct {
	model   *doctype.DocType
	summary bool // read _kora_fact_summary_<model>; otherwise read the fact tables
	grain   Grain
	from    tenanttime.Date
	to      tenanttime.Date
	dims    []string    // dimension columns the result is grouped by
	series  []series    // one aggregate per simple metric, shared by every output that uses it
	outputs []output    // one per requested metric, in request order
	where   []predicate // query filters, applied to every series
	scope   Scope
}

// series is one aggregate of one simple metric. Its own filters apply only to that series.
type series struct {
	metric string
	agg    string
	column string // measure column for sum, dimension for count_distinct, empty for count
	conds  []predicate
}

// output is one requested metric. A simple metric reads series num; a ratio divides num by den.
type output struct {
	metric string
	kind   Kind
	num    int
	den    int
}

// predicate compares a column with a value. For status the field is the status field name; for a
// dimension it is the dimension column name.
type predicate struct {
	field string
	op    string
	value any
}

// planQuery checks the query against the catalog and compiles it into a plan.
func planQuery(cat Catalog, q Query) (plan, error) {
	if err := checkShape(q); err != nil {
		return plan{}, err
	}
	p := plan{grain: q.Grain, from: q.From, to: q.To, scope: q.Scope}
	index := map[string]int{}
	for _, name := range q.Metrics {
		m, ok := cat.Metrics[name]
		if !ok {
			return plan{}, fmt.Errorf("unknown metric %q", name)
		}
		if err := p.addOutput(cat, m, index); err != nil {
			return plan{}, err
		}
	}
	if err := p.addGrouping(q); err != nil {
		return plan{}, err
	}
	p.summary = p.fitsSummary()
	return p, nil
}

// checkShape validates the parts of a query that do not depend on the catalog.
func checkShape(q Query) error {
	switch {
	case len(q.Metrics) == 0:
		return errors.New("a query names at least one metric")
	case q.Grain != GrainDay && q.Grain != GrainWeek && q.Grain != GrainMonth:
		return fmt.Errorf("grain %q must be day, week or month", q.Grain)
	case toTime(q.To).Before(toTime(q.From)):
		return fmt.Errorf("range ends %s before it starts %s", q.To, q.From)
	}
	return nil
}

// addOutput compiles one requested metric into outputs and series.
func (p *plan) addOutput(cat Catalog, m Metric, index map[string]int) error {
	if m.Type == KindSimple {
		num, err := p.addSeries(cat, m, index)
		if err != nil {
			return err
		}
		p.outputs = append(p.outputs, output{metric: m.Name, kind: KindSimple, num: num, den: -1})
		return nil
	}
	if m.Type == KindRatio {
		num, err := p.componentSeries(cat, m, m.Numerator, index)
		if err != nil {
			return err
		}
		den, err := p.componentSeries(cat, m, m.Denominator, index)
		if err != nil {
			return err
		}
		p.outputs = append(p.outputs, output{metric: m.Name, kind: KindRatio, num: num, den: den})
		return nil
	}
	return fmt.Errorf("metric %q is %s: %w", m.Name, m.Type, ErrNotSupported)
}

// componentSeries resolves a ratio's numerator or denominator, which must be a simple metric.
func (p *plan) componentSeries(cat Catalog, ratio Metric, name string, index map[string]int) (int, error) {
	m, ok := cat.Metrics[name]
	if !ok {
		return 0, fmt.Errorf("metric %q refers to unknown metric %q", ratio.Name, name)
	}
	if m.Type != KindSimple {
		return 0, fmt.Errorf("ratio %q divides by %s metric %q: %w", ratio.Name, m.Type, name, ErrNotSupported)
	}
	return p.addSeries(cat, m, index)
}

// addSeries adds the aggregate of a simple metric once and returns its index.
func (p *plan) addSeries(cat Catalog, m Metric, index map[string]int) (int, error) {
	if i, ok := index[m.Name]; ok {
		return i, nil
	}
	dt := cat.Models[m.Model]
	if dt == nil || dt.Facts == nil {
		return 0, fmt.Errorf("metric %q: model %q is not a record type with facts", m.Name, m.Model)
	}
	if m.Measure == nil {
		return 0, fmt.Errorf("metric %q has no measure", m.Name)
	}
	if p.model != nil && p.model != dt {
		return 0, fmt.Errorf("metric %q reads %q, not %q: metrics of one query must share a model: %w", m.Name, m.Model, p.model.Name, ErrNotSupported)
	}
	s := series{metric: m.Name, agg: m.Measure.Agg}
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
	p.model = dt
	p.series = append(p.series, s)
	index[m.Name] = len(p.series) - 1
	return index[m.Name], nil
}

// addGrouping checks the dimensions and query filters against the model.
func (p *plan) addGrouping(q Query) error {
	if p.model == nil {
		return errors.New("no metric reads a record type")
	}
	for _, name := range q.Dimensions {
		if !isDimension(p.model.Facts, name) {
			return fmt.Errorf("dimension %q is not a facts dimension of %s", name, p.model.Name)
		}
		p.dims = append(p.dims, name)
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
// status, and per one declared dimension, and they carry no owner. Distinct counts need the facts.
func (p plan) fitsSummary() bool {
	if p.scope.Owner != "" || len(p.dims) > 1 {
		return false
	}
	usable := func(field string) bool {
		return field == p.model.Facts.StatusField || (len(p.dims) == 1 && field == p.dims[0])
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
