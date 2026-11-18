package facts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
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

// Drift checker settings. driftDays is the business days sampled per model; driftWindowDays is how far
// around a day the source is searched, which covers any tenant zone and business-day cutoff.
const (
	driftDays        = 7
	driftWindowDays  = 2
	driftMetricName  = "kora.facts.drift_days"
	driftMetricLabel = "model"
	driftLeaseTable  = "_kora_fact_drift_lease"
	driftLeaseSecs   = 300
	driftEvery       = 10 * time.Minute
)

// errDriftLeaseLost means another replica holds the drift lease now, so this run must not mark anything.
var errDriftLeaseLost = errors.New("facts: drift lease lost")

// driftCounter records drifting days. metric.Int64Counter satisfies it.
type driftCounter interface {
	Add(ctx context.Context, incr int64, options ...metric.AddOption)
}

// NewDriftCounter creates kora.facts.drift_days. Its only attribute is the model: a bounded set, never a tenant.
func NewDriftCounter() (metric.Int64Counter, error) {
	return otel.Meter("kora/facts").Int64Counter(driftMetricName,
		metric.WithUnit("{day}"), metric.WithDescription("Business days where summaries or facts disagreed with the source"))
}

// DriftChecker samples the last driftDays business days of every fact model and checks two things: each
// summary equals the aggregate of its facts, and the live fact headers equal the source records that map to
// the day. A drifting day is marked dirty so the refresher rebuilds its summaries, and it is counted and
// logged. One checker runs per tenant; a lease keeps replicas from checking together, and each mark is made
// only while the caller still holds the lease (checked with its fencing token).
type DriftChecker struct {
	DB        *sql.DB
	Dialect   db.Dialect
	Registry  *doctype.Registry
	Site      string
	Admission interface {
		AcquireMaintenance(context.Context) (func(), error)
	}
	Owner   string
	Drifted driftCounter     // optional
	Now     func() time.Time // optional; the clock the sample is taken from (default time.Now)

	mu    sync.Mutex
	ready bool
	token int64 // fencing token of the lease this run holds
}

// RunDue checks every model once and reports how many sampled days drifted. A full maintenance lane or another
// replica's lease defers the check.
func (c *DriftChecker) RunDue(ctx context.Context) (int, error) {
	release, err := c.Admission.AcquireMaintenance(ctx)
	if errors.Is(err, dbaccess.ErrExhausted) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("facts: admit drift check: %w", err)
	}
	defer release()
	claimed, err := c.claim(ctx)
	if err != nil || !claimed {
		return 0, err
	}
	defer c.release(ctx)
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
	if err := c.drainRebuilds(ctx); err != nil {
		return drifted, err
	}
	return drifted, nil
}

// drainRebuilds runs the queued rebuild jobs of every model under this lease, so a repair queued by the check
// is applied without an operator. Jobs are claimed one at a time: a crashed run leaves its job resumable.
func (c *DriftChecker) drainRebuilds(ctx context.Context) error {
	store := &JobStore{DB: c.DB, Dialect: c.Dialect}
	rebuilder := &Rebuilder{DB: c.DB, Dialect: c.Dialect, Registry: c.Registry, Site: c.Site, Store: store}
	for _, dt := range c.models() {
		for {
			job, err := store.Claim(ctx, dt.Name, c.Owner)
			if err != nil {
				return err
			}
			if job == nil {
				break
			}
			if err := rebuilder.Run(ctx, job); err != nil {
				return err
			}
		}
	}
	return nil
}

// Run checks until ctx ends, pausing driftEvery plus up to half of it as jitter.
func (c *DriftChecker) Run(ctx context.Context) {
	for {
		if _, err := c.RunDue(ctx); err != nil && ctx.Err() == nil {
			slog.Error("fact drift check", "error", err)
		}
		pause := driftEvery + rand.N(driftEvery/2) //nolint:gosec // jitter spreads checks; no security use
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
	}
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
		summaryOff, err := c.summaryDrift(ctx, dt, day)
		if err != nil {
			return drifted, err
		}
		sourceOff, err := c.sourceDrift(ctx, dt, day)
		if err != nil {
			return drifted, err
		}
		if !summaryOff && !sourceOff {
			continue
		}
		slog.Warn("fact drift", "model", dt.Name, "day", day.String(), "summary_drift", summaryOff, "source_drift", sourceOff)
		if held, err := c.held(ctx); err != nil || !held {
			if err == nil {
				err = errDriftLeaseLost
			}
			return drifted, err
		}
		if err := c.markDirtyDay(ctx, dt, day); err != nil {
			return drifted, err
		}
		if sourceOff {
			if err := c.queueRepair(ctx, dt, day); err != nil {
				return drifted, err
			}
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

// sourceDrift reports whether the live fact headers of day differ from the source records that map to it.
// Expected rows come from facts.Map over the source, so an excluded status counts as not live.
func (c *DriftChecker) sourceDrift(ctx context.Context, dt *doctype.DocType, day tenanttime.Date) (bool, error) {
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("facts: begin source check: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	names, err := c.candidates(ctx, tx, dt, day)
	if err != nil {
		return false, err
	}
	expected, err := c.expectedHeaders(ctx, tx, dt, day, names)
	if err != nil {
		return false, err
	}
	actual, err := c.liveHeaders(ctx, dt, day)
	if err != nil {
		return false, err
	}
	return !maps.Equal(expected, actual), nil
}

// expectedHeaders maps the candidate source records to the live header rows they should produce on day.
func (c *DriftChecker) expectedHeaders(ctx context.Context, tx *sql.Tx, dt *doctype.DocType, day tenanttime.Date, names []string) (map[string]string, error) {
	expected := map[string]string{}
	if len(names) == 0 {
		return expected, nil
	}
	clock, err := tenanttime.Load(ctx, tx, c.Dialect, c.Site)
	if err != nil {
		return nil, fmt.Errorf("facts: load tenant clock: %w", err)
	}
	records, err := (&Rebuilder{DB: c.DB, Dialect: c.Dialect, Registry: c.Registry, Site: c.Site}).records(ctx, tx, dt, names)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		row := records[name]
		head, _ := Map(dt, Change{RecordID: name, Owner: text(row["owner"]), Created: instant(row["creation"]), Data: row}, clock)
		if head.BusinessDate == day && !head.Deleted {
			expected[name] = canonicalHeader(dt, head)
		}
	}
	return expected, nil
}

// candidates lists the source records whose axis falls within driftWindowDays of day.
func (c *DriftChecker) candidates(ctx context.Context, tx *sql.Tx, dt *doctype.DocType, day tenanttime.Date) ([]string, error) {
	axis := c.Dialect.QuoteIdent(dt.Facts.Time)
	start, end := windowBounds(Window{From: day, To: day})
	query := fmt.Sprintf("SELECT name FROM %s WHERE %s >= ? AND %s < ?", c.Dialect.QuoteIdent(dt.RawTableName()), axis, axis) //nolint:gosec // identifiers only, quoted by the dialect
	rows, err := tx.QueryContext(ctx, db.Rebind(c.Dialect, query), start, end)
	if err != nil {
		return nil, fmt.Errorf("facts: read source window: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("facts: scan source window: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// liveHeaders returns the live fact headers of day, keyed by record, in the canonical form canonicalHeader uses.
func (c *DriftChecker) liveHeaders(ctx context.Context, dt *doctype.DocType, day tenanttime.Date) (map[string]string, error) {
	cols := []string{"record_id", "status", "legal_entity", "owner"}
	for _, m := range dt.Facts.Measures {
		cols = append(cols, m.ColumnName())
	}
	for _, d := range dt.Facts.Dimensions {
		cols = append(cols, d.ColumnName())
	}
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s = '' AND %s = ? AND %s = ?", quoteList(c.Dialect, cols),
		c.Dialect.QuoteIdent(dt.Facts.TableName(dt)), c.Dialect.QuoteIdent("line_id"), c.Dialect.QuoteIdent("business_date"), c.Dialect.QuoteIdent("deleted"))
	rows, err := c.DB.QueryContext(ctx, db.Rebind(c.Dialect, query), day.String(), db.BoolParam(c.Dialect, false))
	if err != nil {
		return nil, fmt.Errorf("facts: read live headers: %w", err)
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
			return nil, fmt.Errorf("facts: scan live header: %w", err)
		}
		parts := make([]string, 0, len(vals)-1)
		for _, v := range vals[1:] {
			parts = append(parts, normalised(v))
		}
		out[normalised(vals[0])] = strings.Join(parts, "|")
	}
	return out, rows.Err()
}

// queueRepair queues a rebuild covering day, unless a queued or running job of the model already covers it.
// The caller holds the drift lease.
func (c *DriftChecker) queueRepair(ctx context.Context, dt *doctype.DocType, day tenanttime.Date) error {
	store := &JobStore{DB: c.DB, Dialect: c.Dialect}
	covered, err := store.covers(ctx, dt.Name, day)
	if err != nil || covered {
		return err
	}
	_, err = store.Enqueue(ctx, dt.Name, Window{From: day, To: day})
	return err
}

// markDirtyDay marks one business day dirty. The caller has checked the lease.
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

// canonicalHeader renders a live header row as the same string liveHeaders builds from the table.
func canonicalHeader(dt *doctype.DocType, head Row) string {
	parts := []string{normalised(head.Status), normalised(head.LegalEntity), normalised(head.Owner)}
	for _, m := range dt.Facts.Measures {
		parts = append(parts, normalised(head.Measures[m.ColumnName()].String()))
	}
	for _, d := range dt.Facts.Dimensions {
		parts = append(parts, normalised(head.Dimensions[d.ColumnName()]))
	}
	return strings.Join(parts, "|")
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

// claim takes the tenant's drift lease if it is free and records the fencing token of the claim. Lease
// times come from the database clock, and every claim raises the token, so an old holder can be told apart.
func (c *DriftChecker) claim(ctx context.Context) (bool, error) {
	if err := c.ensureLease(ctx); err != nil {
		return false, err
	}
	leaseExpr, leaseArg := dbClock(c.Dialect, driftLeaseSecs)
	nowExpr, nowArg := dbClock(c.Dialect, 0)
	query := fmt.Sprintf("UPDATE %s SET owner = ?, fencing_token = fencing_token + 1, lease_until = %s WHERE id = 1 AND lease_until <= %s",
		c.Dialect.QuoteIdent(driftLeaseTable), leaseExpr, nowExpr)
	res, err := c.DB.ExecContext(ctx, db.Rebind(c.Dialect, query), c.Owner, leaseArg, nowArg)
	if err != nil {
		return false, fmt.Errorf("facts: claim drift lease: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return false, err
	}
	read := fmt.Sprintf("SELECT fencing_token FROM %s WHERE id = 1 AND owner = ?", c.Dialect.QuoteIdent(driftLeaseTable))
	if err = c.DB.QueryRowContext(ctx, db.Rebind(c.Dialect, read), c.Owner).Scan(&c.token); err != nil {
		return false, fmt.Errorf("facts: read drift fencing token: %w", err)
	}
	return true, nil
}

// held renews the lease if this run still owns it at its fencing token, and reports whether it does.
// It is the check before every mark: an expired or taken lease fails the conditional UPDATE.
func (c *DriftChecker) held(ctx context.Context) (bool, error) {
	leaseExpr, leaseArg := dbClock(c.Dialect, driftLeaseSecs)
	nowExpr, nowArg := dbClock(c.Dialect, 0)
	query := fmt.Sprintf("UPDATE %s SET lease_until = %s WHERE id = 1 AND owner = ? AND fencing_token = ? AND lease_until > %s",
		c.Dialect.QuoteIdent(driftLeaseTable), leaseExpr, nowExpr)
	res, err := c.DB.ExecContext(ctx, db.Rebind(c.Dialect, query), leaseArg, c.Owner, c.token, nowArg)
	if err != nil {
		return false, fmt.Errorf("facts: renew drift lease: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// release ends the lease this checker holds, so the next scan can claim it at once.
func (c *DriftChecker) release(ctx context.Context) {
	nowExpr, nowArg := dbClock(c.Dialect, 0)
	query := fmt.Sprintf("UPDATE %s SET lease_until = %s WHERE id = 1 AND owner = ?", c.Dialect.QuoteIdent(driftLeaseTable), nowExpr)
	if _, err := c.DB.ExecContext(ctx, db.Rebind(c.Dialect, query), nowArg, c.Owner); err != nil {
		slog.Warn("release fact drift lease", "error", err)
	}
}

// ensureLease creates the lease table and its single row once per checker. The row seed is a no-op on
// conflict, so replicas starting together cannot reset a lease another replica holds.
func (c *DriftChecker) ensureLease(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ready {
		return nil
	}
	cols := []string{columnDef(c.Dialect, "id", "Int"), columnDef(c.Dialect, "owner", "Data"), columnDef(c.Dialect, "fencing_token", "Int"),
		columnDef(c.Dialect, "lease_until", "Datetime"), fmt.Sprintf("PRIMARY KEY (%s)", c.Dialect.QuoteIdent("id"))}
	ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n  %s\n)", c.Dialect.QuoteIdent(driftLeaseTable), strings.Join(cols, ",\n  "))
	if suffix := c.Dialect.TableSuffix(); suffix != "" {
		ddl += " " + suffix
	}
	if _, err := c.DB.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("facts: create drift lease table: %w", err)
	}
	seed := fmt.Sprintf("INSERT INTO %s (id, owner, fencing_token, lease_until) VALUES (1, '', 0, %s) %s", c.Dialect.QuoteIdent(driftLeaseTable), //nolint:gosec // identifiers and fixed clauses only, quoted by the dialect
		c.Dialect.NowTimestamp(), c.Dialect.UpsertClause([]string{"id"}, []string{"id"}))
	if _, err := c.DB.ExecContext(ctx, seed); err != nil {
		return fmt.Errorf("facts: seed drift lease: %w", err)
	}
	c.ready = true
	return nil
}
