package facts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// driftDays is the number of recent business days sampled per model.
const (
	driftDays        = 7
	driftMetricName  = "kora.facts.drift_days"
	driftMetricLabel = "model"
)

// driftCounter records drifting days. metric.Int64Counter satisfies it.
type driftCounter interface {
	Add(ctx context.Context, incr int64, options ...metric.AddOption)
}

// NewDriftCounter creates kora.facts.drift_days. Its only attribute is the model: a bounded set, never a tenant.
func NewDriftCounter() (metric.Int64Counter, error) {
	return otel.Meter("kora/facts").Int64Counter(driftMetricName,
		metric.WithUnit("{day}"), metric.WithDescription("Business days where summaries disagreed with the facts"))
}

// DriftChecker samples recent days of every fact model and checks that each summary equals the aggregate of
// the facts. A drifting day is marked dirty so the refresher rebuilds it, and it is counted and logged.
type DriftChecker struct {
	DB        *sql.DB
	Dialect   db.Dialect
	Registry  *doctype.Registry
	Admission interface {
		AcquireMaintenance(context.Context) (func(), error)
	}
	Drifted driftCounter     // optional
	Site    string           // tenant whose clock picks the sampled days
	Now     func() time.Time // optional; the clock the sample is taken from (default time.Now)
}

// RunDue checks every model once and reports how many sampled days drifted. A full maintenance lane defers the check.
func (c *DriftChecker) RunDue(ctx context.Context) (int, error) {
	release, err := c.Admission.AcquireMaintenance(ctx)
	if errors.Is(err, dbaccess.ErrExhausted) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("facts: admit drift check: %w", err)
	}
	defer release()
	days, err := c.sampleDays(ctx)
	if err != nil {
		return 0, err
	}
	drifted := 0
	for _, dt := range c.models() {
		n, err := c.checkModel(ctx, dt, days)
		drifted += n
		if err != nil {
			return drifted, err
		}
	}
	return drifted, nil
}

// models lists the fact models in name order, so each run visits them the same way.
func (c *DriftChecker) models() []*doctype.DocType {
	var out []*doctype.DocType
	for _, dt := range c.Registry.All() {
		if dt.Facts != nil {
			out = append(out, dt)
		}
	}
	slices.SortFunc(out, func(a, b *doctype.DocType) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// checkModel compares the sampled days of one model and marks each drifting day dirty.
func (c *DriftChecker) checkModel(ctx context.Context, dt *doctype.DocType, days []tenanttime.Date) (int, error) {
	drifted := 0
	for _, day := range days {
		off, err := c.summaryDrift(ctx, dt, day)
		if err != nil {
			return drifted, err
		}
		if !off {
			continue
		}
		slog.Warn("fact drift", "model", dt.Name, "day", day.String())
		if err := c.markDirtyDay(ctx, dt, day); err != nil {
			return drifted, err
		}
		if c.Drifted != nil {
			c.Drifted.Add(ctx, 1, metric.WithAttributes(attribute.String(driftMetricLabel, dt.Name)))
		}
		drifted++
	}
	return drifted, nil
}

// sampleDays returns the last driftDays business days on the tenant clock, newest first. A day is sampled
// whether or not it has facts, so a day with source rows and no facts is still checked.
func (c *DriftChecker) sampleDays(ctx context.Context) ([]tenanttime.Date, error) {
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("facts: begin day sample: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	clock, err := tenanttime.Load(ctx, tx, c.Dialect, c.Site)
	if err != nil {
		return nil, fmt.Errorf("facts: load tenant clock: %w", err)
	}
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	today := clock.BusinessDate(now)
	base := time.Date(today.Year, today.Month, today.Day, 0, 0, 0, 0, time.UTC)
	days := make([]tenanttime.Date, driftDays)
	for i := range days {
		d := base.AddDate(0, 0, -i)
		days[i] = tenanttime.Date{Year: d.Year(), Month: d.Month(), Day: d.Day()}
	}
	return days, nil
}

// summaryDrift reports whether the summary rows of day differ from the aggregate of the facts for that day.
func (c *DriftChecker) summaryDrift(ctx context.Context, dt *doctype.DocType, day tenanttime.Date) (bool, error) {
	selectSQL, args := rebuildSelect(c.Dialect, dt, day)
	cols := summaryColumns(dt)
	expected, err := c.summaryRows(ctx, cols, fmt.Sprintf("SELECT %s FROM (%s) AS e", quoteList(c.Dialect, cols), selectSQL), args...)
	if err != nil {
		return false, err
	}
	actual, err := c.summaryRows(ctx, cols, fmt.Sprintf("SELECT %s FROM %s WHERE %s = ?",
		quoteList(c.Dialect, cols), c.Dialect.QuoteIdent(summaryTable(dt)), c.Dialect.QuoteIdent("business_date")), day.String())
	if err != nil {
		return false, err
	}
	return !maps.Equal(expected, actual), nil
}

// summaryRows reads summary-shaped rows keyed by status, grain, grain value and legal entity. The value is
// row_count and each measure, normalised so MySQL and Postgres text agree.
func (c *DriftChecker) summaryRows(ctx context.Context, cols []string, query string, args ...any) (map[string]string, error) {
	rows, err := c.DB.QueryContext(ctx, db.Rebind(c.Dialect, query), args...)
	if err != nil {
		return nil, fmt.Errorf("facts: read summary rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err = rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("facts: scan summary row: %w", err)
		}
		key := fmt.Sprintf("%s/%s/%s/%s", normalised(vals[2]), normalised(vals[3]), normalised(vals[4]), normalised(vals[1]))
		values := make([]string, 0, len(vals)-5)
		for _, v := range vals[5:] {
			values = append(values, normalised(v))
		}
		out[key] = strings.Join(values, "|")
	}
	return out, rows.Err()
}

// markDirtyDay marks one business day dirty so the refresher rebuilds its summaries.
func (c *DriftChecker) markDirtyDay(ctx context.Context, dt *doctype.DocType, day tenanttime.Date) error {
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("facts: begin drift mark: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	b := &Builder{DB: c.DB, Dialect: c.Dialect, Registry: c.Registry}
	err = b.markDirty(ctx, tx, dt.Name, day)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// normalised renders a SQL value canonically: numbers as decimals, anything else as text. NULL is empty.
func normalised(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		v = string(x)
	}
	s := fmt.Sprint(v)
	if d, err := decimal.NewFromString(s); err == nil {
		return d.String()
	}
	return s
}
