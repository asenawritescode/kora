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
// Doctype. The list API applies the caller's own record scope.
type Drill struct {
	Doctype string
	Filters []Filter
}

// drillTimeLayout is the datetime text the list API compares the time field with: UTC, to the second.
const drillTimeLayout = "2006-01-02 15:04:05"

// Drill turns a simple sum or count into the record list it summarizes. Only a header measure
// drills: a sum over a line field would list whole records, which does not add up to the number.
// Business days map to instants through clock, so the record time field is compared in UTC.
func (c Catalog) Drill(req DrillRequest, clock tenanttime.Clock) (Drill, error) {
	m, ok := c.Metrics[req.Metric]
	if !ok {
		return Drill{}, fmt.Errorf("metric %q is not defined", req.Metric)
	}
	if m.Type != KindSimple {
		return Drill{}, fmt.Errorf("metric %q is %s: only simple metrics drill to records", m.Name, m.Type)
	}
	dt := c.Models[m.Model]
	if dt == nil || dt.Facts == nil {
		return Drill{}, fmt.Errorf("metric %q reads %q, which has no fact model", m.Name, m.Model)
	}
	f := dt.Facts
	if m.Measure == nil || m.Measure.Agg == aggCountDistinct || (m.Measure.Agg == aggSum && lineField(f, m.Measure.Field)) {
		return Drill{}, fmt.Errorf("metric %q does not drill to records", m.Name)
	}
	if toTime(req.To).Before(toTime(req.From)) {
		return Drill{}, fmt.Errorf("range ends %s before it starts %s", req.To, req.From)
	}
	out := Drill{Doctype: dt.Name}
	out.Filters = append(out.Filters, m.Filters...)
	for _, dim := range slices.Sorted(maps.Keys(req.Group)) {
		if !headerDimension(f, dim) {
			return Drill{}, fmt.Errorf("%q is not a record dimension of %q", dim, m.Model)
		}
		out.Filters = append(out.Filters, Filter{Field: dim, Op: "=", Value: req.Group[dim]})
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
	for _, d := range f.Dimensions {
		if d.ColumnName() == column {
			return !fieldPath(d.Field).isLine()
		}
	}
	return false
}
