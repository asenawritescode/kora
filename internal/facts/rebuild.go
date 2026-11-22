package facts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// rebuildChunk is the number of source records one transaction rebuilds.
const rebuildChunk = 1000

// Rebuilder fills one claimed job's fact tables from the source tables, one chunk per transaction.
// Each chunk advances the checkpoint first, so a lost lease rolls back that chunk's rows.
type Rebuilder struct {
	DB       *sql.DB
	Dialect  db.Dialect
	Registry *doctype.Registry
	Site     string
	Store    *JobStore

	// chunkSize overrides rebuildChunk in tests; afterChunk runs after each committed chunk.
	chunkSize  int
	afterChunk func()
}

// Run rebuilds job from its checkpoint to the end of its source table, then marks it done. A cancelled
// context leaves the job running, so a later claim resumes it. Any other failure marks it failed.
func (r *Rebuilder) Run(ctx context.Context, job *Job) error {
	dt := r.Registry.Get(job.Model)
	if dt == nil || dt.Facts == nil {
		return fmt.Errorf("facts: no fact model for doctype %q", job.Model)
	}
	if err := r.builder().ensure(ctx, dt); err != nil {
		return r.fail(ctx, job, err)
	}
	if err := r.size(ctx, job, dt); err != nil {
		return r.fail(ctx, job, err)
	}
	for {
		more, err := r.chunk(ctx, job, dt)
		if err != nil {
			return r.fail(ctx, job, err)
		}
		if !more {
			break
		}
		if r.afterChunk != nil {
			r.afterChunk()
		}
	}
	return r.Store.Finish(ctx, job, JobDone, "")
}

// size counts the source records the job covers and stores the count as its total, so progress has a
// denominator. A resumed job counts again: the total then reflects the source as it is now.
func (r *Rebuilder) size(ctx context.Context, job *Job, dt *doctype.DocType) error {
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s", r.Dialect.QuoteIdent(dt.RawTableName())) //nolint:gosec // identifiers only, quoted by the dialect
	var args []any
	if !job.Window.isZero() {
		axis := r.Dialect.QuoteIdent(dt.Facts.Time)
		start, end := windowBounds(job.Window)
		query += fmt.Sprintf(" WHERE %s >= ? AND %s < ?", axis, axis) //nolint:gosec // identifiers only, quoted by the dialect
		args = []any{start, end}
	}
	var total int64
	if err := r.DB.QueryRowContext(ctx, db.Rebind(r.Dialect, query), args...).Scan(&total); err != nil {
		return fmt.Errorf("facts: count rebuild source: %w", err)
	}
	return r.Store.setTotal(ctx, job, total)
}

// fail marks job failed unless the cause is a lost lease or a cancelled context.
func (r *Rebuilder) fail(ctx context.Context, job *Job, cause error) error {
	if ctx.Err() != nil || errors.Is(cause, ErrLeaseLost) {
		return cause
	}
	if err := r.Store.Finish(ctx, job, JobFailed, cause.Error()); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// builder returns the fact writer the rebuild uses. It holds no state beyond its handles, so each caller
// may take its own.
func (r *Rebuilder) builder() *Builder {
	return &Builder{DB: r.DB, Dialect: r.Dialect, Registry: r.Registry}
}

// chunk rebuilds the next chunk of source records after the checkpoint. It reports whether any remained.
func (r *Rebuilder) chunk(ctx context.Context, job *Job, dt *doctype.DocType) (bool, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("facts: begin rebuild chunk: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	size := r.chunkSize
	if size == 0 {
		size = rebuildChunk
	}
	names, err := r.names(ctx, tx, dt, job, size)
	if err != nil || len(names) == 0 {
		return false, err
	}
	err = r.writeChunk(ctx, tx, job, dt, names)
	if err != nil {
		return false, err
	}
	err = tx.Commit()
	if err != nil {
		return false, fmt.Errorf("facts: commit rebuild chunk: %w", err)
	}
	job.Checkpoint = names[len(names)-1]
	job.Processed += int64(len(names))
	return true, nil
}

// writeChunk advances the checkpoint past names, then maps those source records to fact rows, all in tx.
func (r *Rebuilder) writeChunk(ctx context.Context, tx *sql.Tx, job *Job, dt *doctype.DocType, names []string) error {
	if err := r.Store.advance(ctx, tx, job, names[len(names)-1], int64(len(names))); err != nil {
		return err
	}
	clock, err := tenanttime.Load(ctx, tx, r.Dialect, r.Site)
	if err != nil {
		return fmt.Errorf("facts: load tenant clock: %w", err)
	}
	records, err := r.records(ctx, tx, dt, names)
	if err != nil {
		return err
	}
	b := r.builder()
	now := time.Now().UTC()
	for _, name := range names {
		row := records[name]
		change := Change{RecordID: name, Owner: text(row["owner"]), Created: instant(row["creation"]),
			Sequence: job.StartSequence, OccurredAt: now, Data: row}
		head, lines, err := Map(dt, change, clock)
		if err != nil {
			return fmt.Errorf("facts: %s %s: %w", dt.Name, name, err)
		}
		if head.BusinessDate == (tenanttime.Date{}) {
			return fmt.Errorf("facts: %s %s has no business date on axis %q", dt.Name, name, dt.Facts.Time)
		}
		if err := b.writeRows(ctx, tx, dt, head, lines); err != nil {
			return err
		}
		if err := b.markDirtyDates(ctx, tx, dt.Name, []tenanttime.Date{head.BusinessDate}); err != nil {
			return err
		}
	}
	return nil
}

// windowBounds is the source axis range searched for a window: driftWindowDays either side, end exclusive.
func windowBounds(w Window) (time.Time, time.Time) {
	start := time.Date(w.From.Year, w.From.Month, w.From.Day, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -driftWindowDays)
	end := time.Date(w.To.Year, w.To.Month, w.To.Day, 0, 0, 0, 0, time.UTC).AddDate(0, 0, driftWindowDays+1)
	return start, end
}

// names returns the next size record names after checkpoint, in name order.
func (r *Rebuilder) names(ctx context.Context, tx *sql.Tx, dt *doctype.DocType, job *Job, size int) ([]string, error) {
	axis := r.Dialect.QuoteIdent(dt.Facts.Time)
	query := fmt.Sprintf("SELECT name FROM %s WHERE name > ?", r.Dialect.QuoteIdent(dt.RawTableName())) //nolint:gosec // identifiers only, quoted by the dialect
	args := []any{job.Checkpoint}
	if !job.Window.isZero() {
		start, end := windowBounds(job.Window)
		query += fmt.Sprintf(" AND %s >= ? AND %s < ?", axis, axis) //nolint:gosec // identifiers only, quoted by the dialect
		args = append(args, start, end)
	}
	query += " ORDER BY name LIMIT ?"
	rows, err := tx.QueryContext(ctx, db.Rebind(r.Dialect, query), append(args, size)...)
	if err != nil {
		return nil, fmt.Errorf("facts: read rebuild chunk: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("facts: scan rebuild chunk: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// records loads the parent rows named in names and, when the model has lines, their child rows under
// the lines field, as the consumer's payload carries them.
func (r *Rebuilder) records(ctx context.Context, tx *sql.Tx, dt *doctype.DocType, names []string) (map[string]map[string]any, error) {
	marks, args := inList(names)
	parents, err := r.selectMaps(ctx, tx, fmt.Sprintf("SELECT * FROM %s WHERE name IN (%s)", r.Dialect.QuoteIdent(dt.RawTableName()), marks), args)
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]any, len(parents))
	for _, row := range parents {
		out[text(row["name"])] = row
	}
	lines := dt.Facts.Lines
	if lines == "" {
		return out, nil
	}
	children, err := r.selectMaps(ctx, tx, fmt.Sprintf("SELECT * FROM %s WHERE parentfield = ? AND parent IN (%s)",
		r.Dialect.QuoteIdent(dt.RawChildTableName(lines)), marks), append([]any{lines}, args...))
	if err != nil {
		return nil, err
	}
	for _, child := range children {
		parent := out[text(child["parent"])]
		parent[lines] = append(childRows(parent, lines), child)
	}
	return out, nil
}

// selectMaps runs query and returns every row keyed by column name.
func (r *Rebuilder) selectMaps(ctx context.Context, tx *sql.Tx, query string, args []any) ([]map[string]any, error) {
	rows, err := tx.QueryContext(ctx, db.Rebind(r.Dialect, query), args...)
	if err != nil {
		return nil, fmt.Errorf("facts: read rebuild rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("facts: read rebuild columns: %w", err)
	}
	var out []map[string]any
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err = rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("facts: scan rebuild row: %w", err)
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = values[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// advance moves the checkpoint and renews the lease of the job the caller holds, inside tx. It is the
// first write of a chunk, so a lost lease rolls the chunk back.
func (s *JobStore) advance(ctx context.Context, tx *sql.Tx, job *Job, checkpoint string, n int64) error {
	leaseExpr, leaseArg := dbClock(s.Dialect, leaseSeconds)
	nowExpr, nowArg := dbClock(s.Dialect, 0)
	query := fmt.Sprintf(`UPDATE %s SET checkpoint = ?, processed = processed + ?, lease_until = %s, updated_at = ?
		WHERE id = ? AND status = ? AND lease_owner = ? AND fencing_token = ? AND lease_until > %s`, s.table(), leaseExpr, nowExpr)
	args := []any{checkpoint, n, leaseArg, time.Now().UTC(), job.ID, string(JobRunning), job.owner, job.token, nowArg}
	result, err := tx.ExecContext(ctx, db.Rebind(s.Dialect, query), args...)
	if err != nil {
		return fmt.Errorf("facts: advance rebuild checkpoint: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("facts: check rebuild checkpoint: %w", err)
	}
	if changed != 1 {
		return ErrLeaseLost
	}
	return nil
}

// setTotal stores the source count of the job the caller holds. updated_at always changes, so a resumed
// job with the same count still reports one changed row.
func (s *JobStore) setTotal(ctx context.Context, job *Job, total int64) error {
	nowExpr, nowArg := dbClock(s.Dialect, 0)
	query := fmt.Sprintf(`UPDATE %s SET total = ?, updated_at = ? WHERE id = ? AND status = ? AND lease_owner = ?
		AND fencing_token = ? AND lease_until > %s`, s.table(), nowExpr)
	args := []any{total, time.Now().UTC(), job.ID, string(JobRunning), job.owner, job.token, nowArg}
	job.Total = total
	return s.leased(ctx, query, args, "set total of")
}
