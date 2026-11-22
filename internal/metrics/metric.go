// Package metrics defines the metrics a pack declares in its metrics/ directory:
// named reads over fact models. Definitions are validated here. plan.go compiles
// simple, ratio and derived metrics; sql.go and run.go run them over the summaries
// and the fact tables; compare.go holds the previous-period window and top-N folding;
// formula.go parses and evaluates derived formulas.
package metrics

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/internal/doctype"
)

// Kind is how a metric is computed.
type Kind string

// Metric kinds.
const (
	KindSimple     Kind = "simple"
	KindRatio      Kind = "ratio"
	KindDerived    Kind = "derived"
	KindBalance    Kind = "balance"
	KindCumulative Kind = "cumulative"
)

// running reports whether a kind accumulates over the buckets of its range: a balance is the value
// as of each bucket's end, a cumulative the total so far. Both are additive, so neither takes count_distinct.
func (k Kind) running() bool { return k == KindBalance || k == KindCumulative }

// Measure aggregates one fact column: sum of a measure, count of rows, or count_distinct of a dimension.
type Measure struct {
	Agg   string `yaml:"agg" json:"agg"`
	Field string `yaml:"field,omitempty" json:"field,omitempty"`
}

// Filter keeps the fact rows whose status or dimension column satisfies Op against Value.
type Filter struct {
	Field string `yaml:"field" json:"field"`
	Op    string `yaml:"op" json:"op"`
	Value any    `yaml:"value" json:"value"`
}

// Metric is one metric definition: one YAML document per metric.
type Metric struct {
	Name        string   `yaml:"name" json:"name"`
	Label       string   `yaml:"label" json:"label"`
	Type        Kind     `yaml:"type" json:"type"`
	Model       string   `yaml:"model,omitempty" json:"model,omitempty"`
	Measure     *Measure `yaml:"measure,omitempty" json:"measure,omitempty"`
	Numerator   string   `yaml:"numerator,omitempty" json:"numerator,omitempty"`
	Denominator string   `yaml:"denominator,omitempty" json:"denominator,omitempty"`
	Formula     string   `yaml:"formula,omitempty" json:"formula,omitempty"`
	Filters     []Filter `yaml:"filters,omitempty" json:"filters,omitempty"`
	Time        string   `yaml:"time,omitempty" json:"time,omitempty"`
	Good        string   `yaml:"good,omitempty" json:"good,omitempty"`
	Format      string   `yaml:"format,omitempty" json:"format,omitempty"`
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`
	Verified    bool     `yaml:"verified,omitempty" json:"verified,omitempty"`
}

var (
	nameRE    = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	identRE   = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	operators = map[string]bool{"=": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true, "in": true}
)

// catalog is what metrics are checked against: the record types of one import and the metrics themselves.
type catalog struct {
	models  map[string]*doctype.DocType
	metrics map[string]Metric
}

// newCatalog indexes the record types and metrics of one import. It also returns the metrics declared twice.
func newCatalog(defs []Metric, doctypes []*doctype.DocType) (catalog, []error) {
	c := catalog{models: map[string]*doctype.DocType{}, metrics: map[string]Metric{}}
	for _, dt := range doctypes {
		// Parsed record types get the fact models the registry will give them, so a pack is checked against what the server runs.
		c.models[dt.Name] = dt.WithGeneratedFacts()
	}
	var errs []error
	for _, m := range defs {
		if _, dup := c.metrics[m.Name]; dup {
			errs = append(errs, fmt.Errorf("metric %q is declared more than once", m.Name))
			continue
		}
		c.metrics[m.Name] = m
	}
	return c, errs
}

// Validate checks metric definitions against the record types of the same import and
// returns every problem at once.
func Validate(defs []Metric, doctypes []*doctype.DocType) error {
	c, errs := newCatalog(defs, doctypes)
	for _, m := range defs {
		if err := m.validate(c); err != nil {
			errs = append(errs, fmt.Errorf("metric %q: %w", m.Name, err))
		}
	}
	errs = append(errs, c.checkCycles())
	return errors.Join(errs...)
}

func (m Metric) validate(c catalog) error {
	switch {
	case !nameRE.MatchString(m.Name):
		return errors.New("name must be lowercase snake_case")
	case strings.TrimSpace(m.Label) == "":
		return errors.New("label is required")
	}
	enums := []struct {
		name, value string
		allowed     []string
	}{
		{"good", m.Good, []string{"", "up", "down"}},
		{"format", m.Format, []string{"", "currency", "number", "percent"}},
		{"time", m.Time, []string{"", "business_date"}},
	}
	for _, e := range enums {
		if !slices.Contains(e.allowed, e.value) {
			return fmt.Errorf("%s %q is not one of %q", e.name, e.value, e.allowed)
		}
	}
	switch m.Type {
	case KindSimple, KindBalance, KindCumulative:
		return m.validateFact(c)
	case KindRatio, KindDerived:
		return m.validateRefs(c)
	}
	return fmt.Errorf("type %q must be simple, ratio, derived, balance or cumulative", m.Type)
}

// validateFact checks a metric that reads one fact model.
func (m Metric) validateFact(c catalog) error {
	if m.Numerator != "" || m.Denominator != "" || m.Formula != "" {
		return errors.New("only ratio and derived metrics take numerator, denominator or formula")
	}
	dt := c.models[m.Model]
	if dt == nil || dt.Facts == nil {
		return fmt.Errorf("model %q is not a record type with facts", m.Model)
	}
	if m.Measure == nil {
		return errors.New("measure is required")
	}
	if err := m.Measure.validate(dt.Facts); err != nil {
		return err
	}
	if err := m.checkAdditive(); err != nil {
		return err
	}
	for _, f := range m.Filters {
		if err := f.validate(dt.Facts); err != nil {
			return err
		}
	}
	return nil
}

// validateRefs checks a ratio or derived metric and that every metric it names exists.
func (m Metric) validateRefs(c catalog) error {
	if err := m.checkRefShape(); err != nil {
		return err
	}
	for _, ref := range m.refs() {
		target, ok := c.metrics[ref]
		if !ok {
			return fmt.Errorf("refers to unknown metric %q", ref)
		}
		if m.Type == KindRatio && target.Type.running() {
			return fmt.Errorf("ratio components must be simple, but %q is %s", ref, target.Type)
		}
	}
	return nil
}

// checkAdditive rejects a count_distinct on a balance or cumulative: distinct counts of each day do not add up.
func (m Metric) checkAdditive() error {
	if m.Type.running() && m.Measure != nil && m.Measure.Agg == aggCountDistinct {
		return fmt.Errorf("a %s metric takes sum or count, not count_distinct", m.Type)
	}
	return nil
}

// checkRefShape checks which fields a ratio or derived metric takes, and that a derived formula parses.
func (m Metric) checkRefShape() error {
	bare := m.Model == "" && m.Measure == nil && len(m.Filters) == 0
	switch m.Type {
	case KindRatio:
		if !bare || m.Formula != "" || m.Numerator == "" || m.Denominator == "" {
			return errors.New("a ratio takes only numerator and denominator")
		}
	case KindDerived:
		if !bare || m.Numerator != "" || m.Denominator != "" || strings.TrimSpace(m.Formula) == "" {
			return errors.New("a derived metric takes only formula")
		}
		_, err := parseFormula(m.Formula)
		return err
	case KindSimple, KindBalance, KindCumulative:
		// Fact metrics name no other metric; validateFact checks their fields.
	}
	return nil
}

// refs lists the metrics this metric is computed from.
func (m Metric) refs() []string {
	switch m.Type {
	case KindRatio:
		return []string{m.Numerator, m.Denominator}
	case KindDerived:
		return identRE.FindAllString(m.Formula, -1)
	case KindSimple, KindBalance, KindCumulative:
		return nil
	}
	return nil
}

// checkCycles reports a metric that depends on itself through ratio or formula references.
func (c catalog) checkCycles() error {
	const visiting, done = 1, 2
	names := make([]string, 0, len(c.metrics))
	for name := range c.metrics {
		names = append(names, name)
	}
	sort.Strings(names)
	state := map[string]int{}
	var visit func(name string) error
	visit = func(name string) error {
		switch state[name] {
		case visiting:
			return fmt.Errorf("metric %q is part of a cycle", name)
		case done:
			return nil
		}
		state[name] = visiting
		for _, ref := range c.metrics[name].refs() {
			if _, ok := c.metrics[ref]; !ok {
				continue // reported by validateRefs
			}
			if err := visit(ref); err != nil {
				return err
			}
		}
		state[name] = done
		return nil
	}
	for _, name := range names {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

// validate checks the aggregation against the fact columns of the model.
func (ms Measure) validate(f *doctype.FactModel) error {
	switch ms.Agg {
	case "count":
		if ms.Field != "" {
			return errors.New("count takes no field")
		}
		return nil
	case "sum":
		for _, m := range f.Measures {
			if m.ColumnName() == ms.Field {
				return nil
			}
		}
		return fmt.Errorf("sum field %q is not a facts measure", ms.Field)
	case "count_distinct":
		if isDimension(f, ms.Field) {
			return nil
		}
		return fmt.Errorf("count_distinct field %q is not a facts dimension", ms.Field)
	}
	return fmt.Errorf("agg %q must be sum, count or count_distinct", ms.Agg)
}

// validate checks a filter against the status field and dimensions of the model.
func (fl Filter) validate(f *doctype.FactModel) error {
	if fl.Field == "" || (fl.Field != f.StatusField && !isDimension(f, fl.Field)) {
		return fmt.Errorf("filter field %q is neither the status field nor a facts dimension", fl.Field)
	}
	if !operators[fl.Op] {
		return fmt.Errorf("filter op %q is not supported", fl.Op)
	}
	list, isList := fl.Value.([]any)
	if fl.Op == "in" && (!isList || len(list) == 0) {
		return errors.New("filter op in needs a non-empty list")
	}
	if fl.Op != "in" && (isList || fl.Value == nil) {
		return fmt.Errorf("filter op %q needs one value", fl.Op)
	}
	return nil
}

func isDimension(f *doctype.FactModel, column string) bool {
	if f.GroupsByStatus(column) {
		return true
	}
	for _, d := range f.Dimensions {
		if d.ColumnName() == column {
			return true
		}
	}
	return false
}
