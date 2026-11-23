package metrics

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// DrillRequest names one simple metric and the group and business days of the number to drill into.
// Group maps a dimension name to its value; an empty Group drills the whole metric.
type DrillRequest struct {
	Metric   string
	From, To tenanttime.Date
	Group    map[string]string
}

// Drill is the record list behind one number: the filters that list its records of the record type
// Doctype. The list API applies the caller's own record scope. ListOpenable is false when a group
// reads a path or a line: the list filters cannot name those, so the list would show another number.
type Drill struct {
	Doctype      string
	Filters      []Filter
	ListOpenable bool
}

// DerivedDrillRefusal is a drill on a ratio or derived metric. Its message names the parts to open instead.
type DerivedDrillRefusal struct{ Message string }

func (r DerivedDrillRefusal) Error() string { return r.Message }

// notDrillable is the refusal for a metric that is not simple: a ratio or derived metric names its parts instead.
func (c Catalog) notDrillable(m Metric) error {
	if m.Type == KindRatio || m.Type == KindDerived {
		return c.derivedRefusal(m)
	}
	return fmt.Errorf("metric %q is %s: only simple metrics drill to records", m.Name, m.Type)
}

// derivedRefusal says what a ratio or derived metric is made of, so the caller opens one of its parts instead.
// A metric with no parts is calculated and has no records of its own.
func (c Catalog) derivedRefusal(m Metric) DerivedDrillRefusal {
	parts := m.Parts()
	if len(parts) == 0 {
		return DerivedDrillRefusal{Message: fmt.Sprintf("%s is calculated, so it has no records of its own.", m.Label)}
	}
	return DerivedDrillRefusal{Message: fmt.Sprintf("%s is %s; open one of them to see records.", m.Label, c.partsInWords(parts))}
}

// partsInWords reads parts as a phrase: "Sales minus Refunds", or "Sales divided by Receipts" for a ratio.
// A part names its metric by label, or by name when the catalog has no label for it.
func (c Catalog) partsInWords(parts []Part) string {
	label := func(p Part) string {
		if l := c.Metrics[p.Metric].Label; l != "" {
			return l
		}
		return p.Metric
	}
	if parts[0].Role != "" {
		return label(parts[0]) + " divided by " + label(parts[1])
	}
	var b strings.Builder
	b.WriteString(label(parts[0]))
	for _, p := range parts[1:] {
		word := " plus "
		if p.Sign == "-" {
			word = " minus "
		}
		b.WriteString(word + label(p))
	}
	return b.String()
}

// drillTimeLayout is the datetime text the list API compares the time field with: UTC, to the second.
const drillTimeLayout = "2006-01-02 15:04:05"

// Drill turns a simple sum or count into the record list it summarizes: the records with at least one line
// that the number counts, see DrillRecords. A distinct count cannot be listed, since its records are not
// the rows it adds up. Groups may follow a path or name a line dimension, and a line measure sums lines; those
// are not list filters, see Drill.ListOpenable.
// Business days map to instants through clock, so the record time field is compared in UTC.
func (c Catalog) Drill(req DrillRequest, clock tenanttime.Clock) (Drill, error) {
	m, ok := c.Metrics[req.Metric]
	if !ok {
		return Drill{}, fmt.Errorf("metric %q is not defined", req.Metric)
	}
	if m.Type != KindSimple {
		return Drill{}, c.notDrillable(m)
	}
	dt := c.Models[m.Model]
	if dt == nil || dt.Facts == nil {
		return Drill{}, fmt.Errorf("metric %q reads %q, which has no fact model", m.Name, m.Model)
	}
	f := dt.Facts
	if m.Measure == nil || m.Measure.Agg == aggCountDistinct {
		return Drill{}, fmt.Errorf("metric %q does not drill to records", m.Name)
	}
	if toTime(req.To).Before(toTime(req.From)) {
		return Drill{}, fmt.Errorf("range ends %s before it starts %s", req.To, req.From)
	}
	// A line measure counts lines the list filters cannot name, so its list would show another number.
	out := Drill{Doctype: dt.Name, ListOpenable: !(m.Measure.Agg == aggSum && lineField(f, m.Measure.Field))}
	out.Filters = append(out.Filters, m.Filters...)
	for _, dim := range slices.Sorted(maps.Keys(req.Group)) {
		if _, _, ok := factDimOf(f, dim); !ok {
			return Drill{}, fmt.Errorf("%q is not a record dimension of %q", dim, m.Model)
		}
		if !headerDimension(f, dim) {
			out.ListOpenable = false
			continue
		}
		field := dim
		if f.GroupsByStatus(dim) {
			field = f.StatusField // the list filters on the status field, not the fact column
		}
		out.Filters = append(out.Filters, Filter{Field: field, Op: "=", Value: req.Group[dim]})
	}
	out.Filters = append(out.Filters, timeFilters(dt, f.Time, req.From, req.To, clock)...)
	return out, nil
}

// timeFilters bounds the record time field to the days From through To. A Date field is compared with
// the days themselves, inclusive. Any other time field (creation, a Datetime) is compared with the
// instants the tenant's days start and end at, in UTC.
func timeFilters(dt *doctype.DocType, field string, from, to tenanttime.Date, clock tenanttime.Clock) []Filter {
	if tf := dt.GetField(field); tf != nil && tf.Fieldtype == "Date" {
		return []Filter{{Field: field, Op: ">=", Value: from.String()}, {Field: field, Op: "<=", Value: to.String()}}
	}
	lo, hi := instants(from, to, clock)
	return []Filter{
		{Field: field, Op: ">=", Value: lo.UTC().Format(drillTimeLayout)},
		{Field: field, Op: "<", Value: hi.UTC().Format(drillTimeLayout)},
	}
}

// instants is the first instant of the day From and the first instant after the day To, in the
// tenant's zone and cutoff. A moment is in a business day exactly when it lies in [lo, hi).
func instants(from, to tenanttime.Date, clock tenanttime.Clock) (lo, hi time.Time) {
	loc := clock.Location
	if loc == nil {
		loc = tenanttime.Default().Location
	}
	lo = time.Date(from.Year, from.Month, from.Day, 0, 0, 0, 0, loc).Add(clock.Cutoff)
	hi = time.Date(to.Year, to.Month, to.Day+1, 0, 0, 0, 0, loc).Add(clock.Cutoff)
	return lo, hi
}

// lineField reports whether a measure column is summed from the child lines of a record type.
func lineField(f *doctype.FactModel, column string) bool {
	for _, m := range f.Measures {
		if m.ColumnName() == column {
			return fieldPath(m.Field).isLine()
		}
	}
	return false
}

// fieldPath is a fact field as written in config: "total" on the record, "items.amount" on a child line.
type fieldPath string

// isLine reports whether the path names a child line field rather than a field of the record itself.
func (p fieldPath) isLine() bool {
	return strings.Contains(string(p), ".")
}

// headerDimension reports whether column is a dimension read from the record itself, not its lines.
func headerDimension(f *doctype.FactModel, column string) bool {
	if f.GroupsByStatus(column) {
		return true
	}
	for _, d := range f.Dimensions {
		if d.ColumnName() == column {
			return !fieldPath(d.Field).isLine()
		}
	}
	return false
}
