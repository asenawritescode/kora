package facts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// refreshLeaseSeconds is a claim's lease; refreshEvery is the scan pause (plus up to half as jitter).
const (
	refreshLeaseSeconds = 60
	refreshEvery        = 30 * time.Second
)

// errLeaseLost reports that the partition was re-marked or retaken during its recompute.
var errLeaseLost = errors.New("facts: summary lease lost")

// Refresher recomputes daily summaries for dirty partitions; markers are durable until refreshed.
type Refresher struct {
	DB        *sql.DB
	Dialect   db.Dialect
	Registry  *doctype.Registry
	Admission interface {
		AcquireMaintenance(context.Context) (func(), error)
	}
	Owner string // lease owner; unique per process
}

// dirtyMarker is one claimed partition and the version its claim saw.
type dirtyMarker struct {
	model   string
	day     tenanttime.Date
	version int64
}

// RefreshDue recomputes the due partitions the maintenance lane admits; a full lane defers the rest.
func (r *Refresher) RefreshDue(ctx context.Context) (int, error) {
	refreshed := 0
	for {
		release, err := r.Admission.AcquireMaintenance(ctx)
		if errors.Is(err, dbaccess.ErrExhausted) {
			return refreshed, nil
		}
		if err != nil {
			return refreshed, fmt.Errorf("facts: admit summary refresh: %w", err)
		}
		mk, ok, err := r.claim(ctx)
		if err == nil && ok {
			err = r.recompute(ctx, mk)
		}
		release()
		switch {
		case errors.Is(err, errLeaseLost):
			return refreshed, nil
		case err != nil:
			return refreshed, err
		case !ok:
			return refreshed, nil
		}
		refreshed++
	}
}

// Run scans for due partitions until ctx ends. Failed scans are logged; their markers stay for the next scan.
func (r *Refresher) Run(ctx context.Context) {
	for {
		if n, err := r.RefreshDue(ctx); err != nil && ctx.Err() == nil {
			slog.Error("refresh fact summaries", "refreshed", n, "error", err)
		}
		pause := refreshEvery + rand.N(refreshEvery/2) //nolint:gosec // jitter spreads scans; no security use
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
	}
}

// claim leases one due marker; lease times come from the database clock.
func (r *Refresher) claim(ctx context.Context) (dirtyMarker, bool, error) {
	table := r.Dialect.QuoteIdent(dirtyTable)
	var mk dirtyMarker
	var rawDay any
	pick := fmt.Sprintf("SELECT model, business_date FROM %s WHERE lease_at IS NULL OR lease_at <= %s ORDER BY business_date, model LIMIT 1",
		table, r.Dialect.NowMinusSeconds())
	err := r.DB.QueryRowContext(ctx, db.Rebind(r.Dialect, pick), refreshLeaseSeconds).Scan(&mk.model, &rawDay)
	if errors.Is(err, sql.ErrNoRows) {
		return dirtyMarker{}, false, nil
	}
	if err != nil {
		return dirtyMarker{}, false, fmt.Errorf("facts: pick dirty partition: %w", err)
	}
	day := instant(rawDay)
	if day.IsZero() {
		return dirtyMarker{}, false, fmt.Errorf("facts: dirty partition %q has no day", mk.model)
	}
	mk.day = tenanttime.Date{Year: day.Year(), Month: day.Month(), Day: day.Day()}
	take := fmt.Sprintf("UPDATE %s SET lease_owner = ?, lease_at = %s WHERE model = ? AND business_date = ? AND (lease_at IS NULL OR lease_at <= %s)",
		table, r.Dialect.NowTimestamp(), r.Dialect.NowMinusSeconds())
	res, err := r.DB.ExecContext(ctx, db.Rebind(r.Dialect, take), r.Owner, mk.model, mk.day.String(), refreshLeaseSeconds)
	if err != nil {
		return dirtyMarker{}, false, fmt.Errorf("facts: lease dirty partition: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return dirtyMarker{}, false, err // n == 0: another refresher took it first; the next scan retries
	}
	read := fmt.Sprintf("SELECT version FROM %s WHERE model = ? AND business_date = ? AND lease_owner = ?", table)
	if err = r.DB.QueryRowContext(ctx, db.Rebind(r.Dialect, read), mk.model, mk.day.String(), r.Owner).Scan(&mk.version); err != nil {
		return dirtyMarker{}, false, fmt.Errorf("facts: read dirty version: %w", err)
	}
	return mk, true, nil
}

// recompute rebuilds one day and clears its marker in one transaction. The clear is fenced by
// owner and version, so a re-mark during the rebuild rolls it back and the day is redone later.
func (r *Refresher) recompute(ctx context.Context, mk dirtyMarker) error {
	dt := r.Registry.Get(mk.model)
	if dt == nil || dt.Facts == nil {
		return fmt.Errorf("facts: summary refresh: no fact model for %q", mk.model)
	}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("facts: begin summary refresh: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // after a successful Commit, Rollback reports sql.ErrTxDone; expected
	del := fmt.Sprintf("DELETE FROM %s WHERE business_date = ?", r.Dialect.QuoteIdent(summaryTable(dt)))
	if _, err = tx.ExecContext(ctx, db.Rebind(r.Dialect, del), mk.day.String()); err != nil {
		return fmt.Errorf("facts: clear summary day: %w", err)
	}
	insert, args := rebuildInsert(r.Dialect, dt, mk.day)
	if _, err = tx.ExecContext(ctx, db.Rebind(r.Dialect, insert), args...); err != nil {
		return fmt.Errorf("facts: write summary day: %w", err)
	}
	fence := fmt.Sprintf("DELETE FROM %s WHERE model = ? AND business_date = ? AND lease_owner = ? AND version = ?", r.Dialect.QuoteIdent(dirtyTable))
	res, err := tx.ExecContext(ctx, db.Rebind(r.Dialect, fence), mk.model, mk.day.String(), r.Owner, mk.version)
	if err != nil {
		return fmt.Errorf("facts: clear dirty marker: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("facts: read dirty marker fence: %w", err)
	}
	if n != 1 {
		return errLeaseLost
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("facts: commit summary refresh: %w", err)
	}
	return nil
}
