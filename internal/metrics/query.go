package metrics

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// ErrNotSupported marks a query that names a metric or shape this compiler does not run yet.
var ErrNotSupported = errors.New("metrics: not supported yet")

// Grain is the width of one result bucket. Weeks start on Monday.
type Grain string

// Bucket widths.
const (
	GrainDay   Grain = "day"
	GrainWeek  Grain = "week"
	GrainMonth Grain = "month"
)

// Scope narrows a query to what the caller may read. An empty field is not applied.
type Scope struct {
	LegalEntity string
	Owner       string
}

// Compare names the window a result is compared with.
type Compare string

// Comparison windows. An empty Compare is CompareNone.
const (
	CompareNone           Compare = "none"
	ComparePreviousPeriod Compare = "previous_period"
)

// Query asks for metrics over a business-date range. From and To are both inclusive.
// Top keeps the best Top groups by the first metric and folds the rest into the Other group; zero
// turns it off, and it needs exactly one dimension. Compare adds the previous period as Result.Previous.
type Query struct {
	Metrics    []string
	Dimensions []string
	Filters    []Filter
	From, To   tenanttime.Date
	Grain      Grain
	Scope      Scope
	Compare    Compare
	Top        int
}

// Catalog is what queries compile against: the metric definitions and the record types they read.
type Catalog struct {
	Metrics map[string]Metric
	Models  map[string]*doctype.DocType
}

// NewCatalog indexes definitions and record types by name.
func NewCatalog(defs []Metric, doctypes []*doctype.DocType) Catalog {
	c := Catalog{Metrics: make(map[string]Metric, len(defs)), Models: make(map[string]*doctype.DocType, len(doctypes))}
	for _, m := range defs {
		c.Metrics[m.Name] = m
	}
	for _, dt := range doctypes {
		c.Models[dt.Name] = dt
	}
	return c
}

// Row is one bucket of one group. Values follow Result.Metrics; Group follows Result.Dimensions.
// A value is null (Valid false) when a ratio's or derived metric's divisor is zero or an operand is
// null; sums and counts are never null. Label is set only on the Other group, whose Group is "__other__".
type Row struct {
	Period tenanttime.Date
	Group  []string
	Label  string
	Values []decimal.NullDecimal
}

// Result holds every bucket of every group in order. Empty buckets are zero. Previous, when the
// query compares, pairs with Rows by position: Previous[i] is the previous-period bucket of Rows[i]
// for the same group.
type Result struct {
	Metrics    []string
	Dimensions []string
	Rows       []Row
	Previous   []Row
}

// periodStart is the first day of the bucket that contains t.
func periodStart(t time.Time, g Grain) time.Time {
	switch g {
	case GrainDay:
		return t
	case GrainWeek:
		return t.AddDate(0, 0, -((int(t.Weekday()) + 6) % 7))
	case GrainMonth:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return t
}

// stepPeriod moves a bucket start by n buckets; a negative n moves back.
func stepPeriod(t time.Time, g Grain, n int) time.Time {
	switch g {
	case GrainWeek:
		return t.AddDate(0, 0, 7*n)
	case GrainMonth:
		return t.AddDate(0, n, 0)
	}
	return t.AddDate(0, 0, n)
}

// bucketStarts lists the start of every bucket that touches [from, to], in order.
func bucketStarts(from, to tenanttime.Date, g Grain) []time.Time {
	var starts []time.Time
	end := toTime(to)
	for b := periodStart(toTime(from), g); !b.After(end); b = stepPeriod(b, g, 1) {
		starts = append(starts, b)
	}
	return starts
}

// toTime places a business date at midnight UTC, so calendar arithmetic never meets a zone.
func toTime(d tenanttime.Date) time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
}

// toDate is the business date of a UTC midnight from toTime.
func toDate(t time.Time) tenanttime.Date {
	return tenanttime.Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}
}
