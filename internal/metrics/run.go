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

// factDirtyTable is the table of business days whose summaries are not yet recomputed: _kora_fact_dirty.
const factDirtyTable = "_kora_fact_dirty"

// defaultTimeout bounds one metrics query, previous period included, when the Engine sets no Timeout.
const defaultTimeout = 10 * time.Second

// groupSep joins dimension values into a map key. Dimension values do not contain NUL.
const groupSep = "\x00"

// openingPeriod keys the one bucket that holds every day before a range, the opening of a balance.
// It is the zero date, which no bucket start is.
var openingPeriod = tenanttime.Date{}

// Engine runs metric queries against one tenant's database.
type Engine struct {
	DB      *sql.DB
	Dialect db.Dialect
	Catalog Catalog
	Timeout time.Duration // zero means defaultTimeout
	// Site is the tenant the query runs for; a dimension of user logins is labelled with that site's users.
	Site string
}

// bucketKey identifies one bucket of one group.
type bucketKey struct {
	period tenanttime.Date
	group  string
}

// bucketSums holds one sum per series for one bucket of one group.
type bucketSums []decimal.Decimal

// add returns the element-wise sum of b and other; a nil b starts from zero.
func (b bucketSums) add(other bucketSums) bucketSums {
	if b == nil {
		b = make(bucketSums, len(other))
	}
	for i, v := range other {
		b[i] = b[i].Add(v)
	}
	return b
}

// at returns the sum of series j, zero when the bucket is missing.
func (b bucketSums) at(j int) decimal.Decimal {
	if j < len(b) {
		return b[j]
	}
	return decimal.Decimal{}
}

// aggregate is the summed series of one run. Groups maps each group key to its dimension values.
type aggregate struct {
	sums   map[bucketKey]bucketSums
	groups map[string][]string
}

// timeout is the bound on one query, zero Timeout meaning defaultTimeout.
func (e Engine) timeout() time.Duration {
	if e.Timeout <= 0 {
		return defaultTimeout
	}
	return e.Timeout
}

// Query compiles q and runs it. A metric or shape this compiler does not run yet fails with
// ErrNotSupported before any SQL is sent.
func (e Engine) Query(ctx context.Context, q Query) (Result, error) {
	p, err := planQuery(e.Catalog, q)
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()
	cur, err := e.run(ctx, p)
	if err != nil {
		return Result{}, err
	}
	if p.top > 0 {
		cur = cur.fold(p.topKeys(cur, p.top))
	}
	return e.result(ctx, p, cur)
}

// result labels an aggregate's groups and builds its rows, with the previous period when the query
// compares.
func (e Engine) result(ctx context.Context, p plan, cur aggregate) (Result, error) {
	keys := p.keys(cur)
	labels, err := e.labelsOf(ctx, p, cur, keys)
	if err != nil {
		return Result{}, err
	}
	res := Result{Metrics: p.metricNames(), Dimensions: p.dimNames(), Rows: p.rows(cur, keys, p.from, p.to, labels)}
	if p.compares() {
		res.Previous, err = e.previousRows(ctx, p, cur, keys, labels)
		if err != nil {
			return Result{}, err
		}
		from, to := p.previousRange()
		res.PreviousSpan = Span{from, to}
	}
	return res, nil
}

// run sums the plan's range. A plan with a balance also sums the days before the range, as the
// opening bucket, so the balance can start from it.
func (e Engine) run(ctx context.Context, p plan) (aggregate, error) {
	p, err := e.settled(ctx, p)
	if err != nil {
		return aggregate{}, err
	}
	agg, err := e.sumRows(ctx, p)
	if err != nil || !p.needsOpening() {
		return agg, err
	}
	opening := p
	opening.opening = true
	before, err := e.sumRows(ctx, opening)
	if err != nil {
		return aggregate{}, err
	}
	for bk, sums := range before.sums {
		agg.sums[bk] = sums
	}
	for key, group := range before.groups {
		agg.groups[key] = group
	}
	return agg, nil
}

// settled reads the fact rows instead of the summary when a day the plan reads is dirty. Summaries are
// recomputed per dirty day, so a dirty day's summary can be older than its fact rows, and the result's
// source sequence claims the fact rows. Clean days are current, so the summary stays the source otherwise.
func (e Engine) settled(ctx context.Context, p plan) (plan, error) {
	if !p.summary {
		return p, nil
	}
	dirty, err := e.hasDirtyDays(ctx, p.model.Name, p.to)
	if err != nil {
		return plan{}, err
	}
	p.summary = !dirty
	return p, nil
}

// hasDirtyDays reports whether a business day of model up to and including through is marked dirty in the
// fact dirty table, which internal/facts writes. It reads the table here, as summaryTable names the summaries,
// because facts imports nothing from metrics.
func (e Engine) hasDirtyDays(ctx context.Context, model string, through tenanttime.Date) (bool, error) {
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE model = ? AND business_date <= ?", e.Dialect.QuoteIdent(factDirtyTable)) //nolint:gosec // identifiers only, quoted by the dialect
	var n int64
	if err := e.DB.QueryRowContext(ctx, db.Rebind(e.Dialect, query), model, through.String()).Scan(&n); err != nil {
		return false, fmt.Errorf("metrics: read dirty days of %s: %w", model, err)
	}
	return n > 0, nil
}

// sumRows sends the plan's statement and sums the rows it returns.
func (e Engine) sumRows(ctx context.Context, p plan) (aggregate, error) {
	stmt, args := p.build(e.Dialect)
	rows, err := e.DB.QueryContext(ctx, db.Rebind(e.Dialect, stmt), args...)
	if err != nil {
		return aggregate{}, fmt.Errorf("metrics: query %s: %w", p.sourceName(), err)
	}
	defer func() { _ = rows.Close() }()
	return p.read(rows)
}

// sourceName names the source for errors: the summary table or the fact table.
func (p plan) sourceName() string {
	if p.summary {
		return p.summaryTable()
	}
	return p.model.Facts.TableName(p.model)
}

// read sums the returned rows into buckets.
func (p plan) read(rows *sql.Rows) (aggregate, error) {
	agg := aggregate{sums: map[bucketKey]bucketSums{}, groups: map[string][]string{}}
	for rows.Next() {
		if err := p.addRow(rows, agg); err != nil {
			return aggregate{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return aggregate{}, fmt.Errorf("metrics: read rows: %w", err)
	}
	return agg, nil
}

// addRow scans one returned row and adds its values to its bucket.
func (p plan) addRow(rows *sql.Rows, agg aggregate) error {
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
	agg.groups[key] = group
	period := openingPeriod
	if !p.opening {
		period = p.bucketOf(t)
	}
	bk := bucketKey{period: period, group: key}
	if agg.sums[bk] == nil {
		agg.sums[bk] = make(bucketSums, len(p.series))
	}
	return p.accumulate(agg.sums[bk], vals)
}

// bucketOf is the key of the bucket that holds business day t. A range holds every day of the
// range in the one bucket keyed by its first day.
func (p plan) bucketOf(t time.Time) tenanttime.Date {
	if p.grain == GrainRange {
		return p.from
	}
	return toDate(periodStart(t, p.grain))
}

// accumulate adds each returned series value to the bucket. A NULL sum adds nothing.
func (p plan) accumulate(bucket bucketSums, vals []sql.NullString) error {
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

// keys lists the group keys a result is built from, in order. A query without dimensions has one
// group, so the total is one group even when no fact matched.
func (p plan) keys(agg aggregate) []string {
	if len(p.dims) == 0 {
		return []string{""}
	}
	keys := make([]string, 0, len(agg.groups))
	for k := range agg.groups {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// rows gap-fills every key across [from, to] and computes each requested metric from its component
// sums. Rows come out key by key, bucket by bucket, so two calls with equal keys and equal bucket
// counts line up by position. labels is nil or one map per dimension, from labelsOf.
func (p plan) rows(agg aggregate, keys []string, from, to tenanttime.Date, labels []map[string]string) []Row {
	starts := bucketStarts(from, to, p.grain)
	out := make([]Row, 0, len(keys)*len(starts))
	for _, key := range keys {
		settled := p.settle(agg, key, starts)
		for i, start := range starts {
			period := toDate(start)
			row := Row{Period: period, Group: agg.groups[key], Values: p.values(settled[i])}
			if labels != nil {
				row.Labels = groupLabels(labels, agg.groups[key])
			}
			if key == otherGroup {
				row.Label = otherLabel
			}
			out = append(out, row)
		}
	}
	return out
}

// settle returns the bucket sums of one group, one entry per start. A balance starts from its
// opening and adds each bucket; a cumulative starts from zero at the range's first day. Every other
// series is the bucket's own sum. A running series' bucket is its value as of the bucket's end; the
// sums hold facts only through To, so a bucket that runs past To reads as of To.
func (p plan) settle(agg aggregate, key string, starts []time.Time) []bucketSums {
	opening := agg.sums[bucketKey{period: openingPeriod, group: key}]
	acc := make(bucketSums, len(p.series))
	for j, s := range p.series {
		if s.kind == KindBalance {
			acc[j] = opening.at(j)
		}
	}
	out := make([]bucketSums, len(starts))
	for i, start := range starts {
		raw := agg.sums[bucketKey{period: toDate(start), group: key}]
		out[i] = make(bucketSums, len(p.series))
		for j, s := range p.series {
			if !s.running() {
				out[i][j] = raw.at(j)
				continue
			}
			acc[j] = acc[j].Add(raw.at(j))
			out[i][j] = acc[j]
		}
	}
	return out
}

// values computes each requested metric of one bucket. Metrics compute in plan order, so a derived
// metric reads values that already exist. A ratio or derived metric is null when a divisor is zero
// or an operand is null. Simple values are the summed value, zero when the bucket is empty.
func (p plan) values(bucket bucketSums) []decimal.NullDecimal {
	computed := make([]decimal.NullDecimal, len(p.metrics))
	for i, o := range p.metrics {
		switch o.kind {
		case KindDerived:
			computed[i] = o.expr.eval(computed)
		case KindRatio:
			if den := bucket[o.den]; !den.IsZero() {
				computed[i] = decimal.NullDecimal{Decimal: bucket[o.num].Div(den), Valid: true}
			}
		default:
			computed[i] = decimal.NullDecimal{Decimal: bucket[o.num], Valid: true}
		}
	}
	vals := make([]decimal.NullDecimal, len(p.requested))
	for i, r := range p.requested {
		vals[i] = computed[r]
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
