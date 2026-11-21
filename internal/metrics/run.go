package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// defaultTimeout bounds one metrics query when the Engine sets no Timeout.
const defaultTimeout = 10 * time.Second

// groupSep joins dimension values into a map key. Dimension values do not contain NUL.
const groupSep = "\x00"

// Engine runs metric queries against one tenant's database.
type Engine struct {
	DB      *sql.DB
	Dialect db.Dialect
	Catalog Catalog
	Timeout time.Duration // zero means defaultTimeout
}

// bucketKey identifies one bucket of one group.
type bucketKey struct {
	period tenanttime.Date
	group  string
}

// Query compiles q and runs it. A metric or shape this compiler does not run yet fails with
// ErrNotSupported before any SQL is sent.
func (e Engine) Query(ctx context.Context, q Query) (Result, error) {
	p, err := planQuery(e.Catalog, q)
	if err != nil {
		return Result{}, err
	}
	stmt, args := p.build(e.Dialect)
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rows, err := e.DB.QueryContext(ctx, db.Rebind(e.Dialect, stmt), args...)
	if err != nil {
		return Result{}, fmt.Errorf("metrics: query %s: %w", p.sourceName(), err)
	}
	defer func() { _ = rows.Close() }()
	sums, groups, err := p.read(rows)
	if err != nil {
		return Result{}, err
	}
	return p.result(sums, groups), nil
}

// sourceName names the source for errors: the summary table or the fact table.
func (p plan) sourceName() string {
	if p.summary {
		return p.summaryTable()
	}
	return p.model.Facts.TableName(p.model)
}

// read sums the returned rows into buckets. Groups maps each group key to its values.
func (p plan) read(rows *sql.Rows) (map[bucketKey][]decimal.Decimal, map[string][]string, error) {
	sums := map[bucketKey][]decimal.Decimal{}
	groups := map[string][]string{}
	for rows.Next() {
		if err := p.addRow(rows, sums, groups); err != nil {
			return nil, nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("metrics: read rows: %w", err)
	}
	return sums, groups, nil
}

// addRow scans one returned row and adds its values to its bucket.
func (p plan) addRow(rows *sql.Rows, sums map[bucketKey][]decimal.Decimal, groups map[string][]string) error {
	var day any
	dims := make([]sql.NullString, len(p.dims))
	vals := make([]sql.NullString, len(p.series))
	dest := append([]any{&day}, nullStringDests(dims)...)
	dest = append(dest, nullStringDests(vals)...)
	if err := rows.Scan(dest...); err != nil {
		return fmt.Errorf("metrics: scan row: %w", err)
	}
	t, err := dateOf(day)
	if err != nil {
		return err
	}
	group := make([]string, len(dims))
	for i, d := range dims {
		group[i] = d.String
	}
	key := strings.Join(group, groupSep)
	groups[key] = group
	bk := bucketKey{period: toDate(periodStart(t, p.grain)), group: key}
	if sums[bk] == nil {
		sums[bk] = make([]decimal.Decimal, len(p.series))
	}
	return p.accumulate(sums[bk], vals)
}

// accumulate adds each returned series value to the bucket. A NULL sum adds nothing.
func (p plan) accumulate(bucket []decimal.Decimal, vals []sql.NullString) error {
	for i, v := range vals {
		if !v.Valid {
			continue
		}
		d, err := decimal.NewFromString(v.String)
		if err != nil {
			return fmt.Errorf("metrics: parse %s value %q: %w", p.series[i].metric, v.String, err)
		}
		bucket[i] = bucket[i].Add(d)
	}
	return nil
}

// nullStringDests returns scan destinations for each value.
func nullStringDests(vals []sql.NullString) []any {
	dests := make([]any, len(vals))
	for i := range vals {
		dests[i] = &vals[i]
	}
	return dests
}

// result gap-fills every group across the range and computes each output from its component sums.
func (p plan) result(sums map[bucketKey][]decimal.Decimal, groups map[string][]string) Result {
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	if len(p.dims) == 0 {
		keys = []string{""} // the total is one group even when no fact matched
	}
	slices.Sort(keys)
	out := Result{Metrics: make([]string, len(p.outputs)), Dimensions: slices.Clone(p.dims)}
	for i, o := range p.outputs {
		out.Metrics[i] = o.metric
	}
	for _, key := range keys {
		for period := periodStart(toTime(p.from), p.grain); !period.After(toTime(p.to)); period = nextPeriod(period, p.grain) {
			bucket := sums[bucketKey{period: toDate(period), group: key}]
			if bucket == nil {
				bucket = make([]decimal.Decimal, len(p.series))
			}
			out.Rows = append(out.Rows, Row{Period: toDate(period), Group: groups[key], Values: p.values(bucket)})
		}
	}
	return out
}

// values computes each output of one bucket. A ratio divides its summed components; a zero
// denominator gives null. Simple outputs are the summed value, zero when the bucket is empty.
func (p plan) values(bucket []decimal.Decimal) []decimal.NullDecimal {
	vals := make([]decimal.NullDecimal, len(p.outputs))
	for i, o := range p.outputs {
		if o.kind != KindRatio {
			vals[i] = decimal.NullDecimal{Decimal: bucket[o.num], Valid: true}
			continue
		}
		if den := bucket[o.den]; !den.IsZero() {
			vals[i] = decimal.NullDecimal{Decimal: bucket[o.num].Div(den), Valid: true}
		}
	}
	return vals
}

// dateOf reads a business date returned by a driver: a time value, or text in YYYY-MM-DD form.
func dateOf(v any) (time.Time, error) {
	switch v := v.(type) {
	case time.Time:
		return time.Date(v.Year(), v.Month(), v.Day(), 0, 0, 0, 0, time.UTC), nil
	case []byte:
		return parseDate(string(v))
	case string:
		return parseDate(v)
	}
	return time.Time{}, fmt.Errorf("metrics: business date has type %T", v)
}

func parseDate(s string) (time.Time, error) {
	if len(s) > 10 {
		s = s[:10]
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("metrics: business date %q: %w", s, err)
	}
	return t, nil
}
