package facts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

const (
	jobTable   = "_kora_fact_rebuild_job"
	guardTable = "_kora_fact_rebuild_guard"
)

// sequenceTable is the outbox sequence row. The outbox takes FOR UPDATE on it in the
// transaction that writes an event, so a locking read here waits for in-flight events.
const sequenceTable = "_kora_tenant_event_sequence"

const leaseSeconds = 45

// JobStatus is the state of a rebuild job.
type JobStatus string

// Job states.
const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobDone      JobStatus = "done"
	JobFailed    JobStatus = "failed"
	JobCancelled JobStatus = "cancelled"
)

var allJobStatuses = []JobStatus{JobQueued, JobRunning, JobDone, JobFailed, JobCancelled}

// jobMoves is the only list of status changes. Claiming (queued, or running with an expired
// lease, to running with a new lease) is guarded separately because it also takes the lease.
var jobMoves = map[JobStatus][]JobStatus{
	JobQueued:  {JobRunning, JobCancelled},
	JobRunning: {JobDone, JobFailed, JobCancelled},
}

// CanMoveTo reports whether a job may move from s to to.
func (s JobStatus) CanMoveTo(to JobStatus) bool {
	return slices.Contains(jobMoves[s], to)
}

// sourcesOf lists every status that may move to to. Each guarded UPDATE names them.
func sourcesOf(to JobStatus) []JobStatus {
	var from []JobStatus
	for _, s := range allJobStatuses {
		if s.CanMoveTo(to) {
			from = append(from, s)
		}
	}
	return from
}

// ErrLeaseLost means the caller no longer holds the job (lease expired and taken over, or cancelled).
var ErrLeaseLost = errors.New("facts: rebuild lease lost")

// Window bounds a rebuild to business days From through To, inclusive. The zero Window rebuilds the model.
type Window struct {
	From, To tenanttime.Date
}

func (w Window) isZero() bool { return w.From == (tenanttime.Date{}) }

// scopeValue is the stored form of one window bound: NULL for the zero Window.
func scopeValue(w Window, d tenanttime.Date) any {
	if w.isZero() {
		return nil
	}
	return d.String()
}

// Job is one rebuild job; owner and token are the lease a Claim took.
type Job struct {
	ID            string
	Model         string
	Window        Window
	Status        JobStatus
	StartSequence int64 // newest outbox sequence the rebuild covers; fixed at enqueue
	Checkpoint    string
	Processed     int64
	Total         int64 // source records in scope, counted when the job first runs
	Error         string
	owner         string
	token         int64
}

// JobStore persists rebuild jobs; writes to a leased job are guarded by status, owner and token.
type JobStore struct {
	DB      *sql.DB
	Dialect db.Dialect

	mu    sync.Mutex
	ready bool
}

// Enqueue stores a queued job for model and captures the outbox sequence under its lock.
func (s *JobStore) Enqueue(ctx context.Context, model string, window Window) (*Job, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("facts: begin rebuild enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	job, err := s.insertQueued(ctx, tx, contract.NewEventID(), model, window)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("facts: commit rebuild enqueue: %w", err)
	}
	return job, nil
}

// insertQueued inserts the queued job id inside tx, capturing the outbox sequence under its lock.
func (s *JobStore) insertQueued(ctx context.Context, tx *sql.Tx, id, model string, window Window) (*Job, error) {
	start, err := readSequence(ctx, tx, s.Dialect)
	if err != nil {
		return nil, err
	}
	job := &Job{ID: id, Model: model, Status: JobQueued, StartSequence: start, Window: window}
	now := time.Now().UTC()
	insert := fmt.Sprintf(`INSERT INTO %s (id, model, status, start_sequence, checkpoint, processed, total, error, lease_owner, fencing_token, lease_until, created_at, updated_at, scope_from, scope_to)
		VALUES (?, ?, ?, ?, '', 0, 0, '', '', 0, %s, ?, ?, ?, ?)`, s.table(), s.Dialect.NowTimestamp())
	args := []any{job.ID, model, string(job.Status), start, now, now, scopeValue(window, window.From), scopeValue(window, window.To)}
	if _, err = tx.ExecContext(ctx, db.Rebind(s.Dialect, insert), args...); err != nil {
		return nil, fmt.Errorf("facts: insert rebuild job: %w", err)
	}
	return job, nil
}

// Get reads one job by id.
func (s *JobStore) Get(ctx context.Context, id string) (*Job, error) {
	query := fmt.Sprintf("SELECT %s FROM %s WHERE id = ?", jobColumns, s.table())
	job, err := scanJob(s.DB.QueryRowContext(ctx, db.Rebind(s.Dialect, query), id))
	if err != nil {
		return nil, fmt.Errorf("facts: read rebuild job %s: %w", id, err)
	}
	return job, nil
}

// Claim takes the oldest queued job of model, or a running job whose lease has expired, and
// returns it with a new lease. It returns nil when there is nothing to claim.
func (s *JobStore) Claim(ctx context.Context, model, owner string) (*Job, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	nowExpr, nowArg := dbClock(s.Dialect, 0)
	query := fmt.Sprintf(`SELECT %s FROM %s WHERE model = ? AND status IN (?, ?) AND lease_until <= %s
		ORDER BY created_at, id LIMIT 1`, jobColumns, s.table(), nowExpr)
	job, err := scanJob(s.DB.QueryRowContext(ctx, db.Rebind(s.Dialect, query),
		model, string(JobQueued), string(JobRunning), nowArg))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("facts: find rebuild job: %w", err)
	}
	// The fencing token is compared too, so a takeover that raced this read is refused.
	leaseExpr, leaseArg := dbClock(s.Dialect, leaseSeconds)
	claim := fmt.Sprintf(`UPDATE %s SET status = ?, lease_owner = ?, fencing_token = ?, lease_until = %s, updated_at = ?
		WHERE id = ? AND fencing_token = ? AND status IN (?, ?) AND lease_until <= %s`, s.table(), leaseExpr, nowExpr)
	args := []any{string(JobRunning), owner, job.token + 1, leaseArg, time.Now().UTC(),
		job.ID, job.token, string(JobQueued), string(JobRunning), nowArg}
	changed, err := s.execRows(ctx, claim, args)
	if err != nil {
		return nil, fmt.Errorf("facts: claim rebuild job: %w", err)
	}
	if changed != 1 {
		return nil, nil
	}
	job.Status, job.owner, job.token = JobRunning, owner, job.token+1
	return job, nil
}

// Renew extends the lease of a running job the caller holds.
func (s *JobStore) Renew(ctx context.Context, job *Job) error {
	leaseExpr, leaseArg := dbClock(s.Dialect, leaseSeconds)
	nowExpr, nowArg := dbClock(s.Dialect, 0)
	query := fmt.Sprintf(`UPDATE %s SET lease_until = %s WHERE id = ? AND status = ? AND lease_owner = ?
		AND fencing_token = ? AND lease_until > %s`, s.table(), leaseExpr, nowExpr)
	args := []any{leaseArg, job.ID, string(JobRunning), job.owner, job.token, nowArg}
	return s.leased(ctx, query, args, "renew")
}

// Finish moves a job the caller holds from running to a terminal state; failure is kept.
func (s *JobStore) Finish(ctx context.Context, job *Job, to JobStatus, failure string) error {
	if !JobRunning.CanMoveTo(to) {
		return fmt.Errorf("facts: rebuild job cannot move from %s to %s", JobRunning, to)
	}
	return s.move(ctx, job, to, "error = ?, updated_at = ?", failure, time.Now().UTC())
}

// covers reports whether a queued or running job of model already rebuilds day: a whole-model job or one
// whose window contains day.
func (s *JobStore) covers(ctx context.Context, model string, day tenanttime.Date) (bool, error) {
	if err := s.ensure(ctx); err != nil {
		return false, err
	}
	marks, statuses := inList([]JobStatus{JobQueued, JobRunning})
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE model = ? AND status IN (%s) AND (scope_from IS NULL OR (scope_from <= ? AND scope_to >= ?))",
		s.table(), marks)
	args := append([]any{model}, statuses...)
	args = append(args, day.String(), day.String())
	var n int
	if err := s.DB.QueryRowContext(ctx, db.Rebind(s.Dialect, query), args...).Scan(&n); err != nil {
		return false, fmt.Errorf("facts: read covering rebuild jobs: %w", err)
	}
	return n > 0, nil
}

// EnqueueOnce queues a whole-model rebuild of model unless one is already queued or running, or has
// finished. A failed or cancelled backfill is queued again. It returns nil when it queued nothing.
//
// The guard row of model is claimed by one conditional UPDATE inside the transaction that inserts the job,
// so two concurrent callers, on this or another replica, queue at most one job.
func (s *JobStore) EnqueueOnce(ctx context.Context, model string) (*Job, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	// A concurrent caller may insert the guard row first; the claim below needs it to exist either way,
	// so the insert's own error is not used. A real database fault fails the claim instead.
	_, _ = s.DB.ExecContext(ctx, db.Rebind(s.Dialect, "INSERT INTO "+s.guardTable()+" (model, job_id) VALUES (?, '')"), model)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("facts: begin backfill enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	id := contract.NewEventID()
	marks, statuses := inList([]JobStatus{JobFailed, JobCancelled})
	claim := fmt.Sprintf(`UPDATE %s SET job_id = ? WHERE model = ? AND (job_id = '' OR job_id IN (SELECT id FROM %s WHERE model = ? AND status IN (%s)))`,
		s.guardTable(), s.table(), marks)
	args := append([]any{id, model, model}, statuses...)
	changed, err := execRowsTx(ctx, tx, db.Rebind(s.Dialect, claim), args)
	if err != nil {
		return nil, fmt.Errorf("facts: claim backfill of %s: %w", model, err)
	}
	if changed != 1 {
		return nil, nil
	}
	job, err := s.insertQueued(ctx, tx, id, model, Window{})
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("facts: commit backfill enqueue: %w", err)
	}
	return job, nil
}

// ReplaceWhole queues a new whole-model rebuild of model after cancelling the queued and running ones, in one
// transaction under the outbox sequence lock. It is the path for a changed fact model: the open job was built
// for the old shape, and a stale worker's next guarded write fails. The guard then points at the new job, so
// a backfill that finished earlier does not block the rebuild the change needs.
func (s *JobStore) ReplaceWhole(ctx context.Context, model string) (*Job, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	// The guard row may be created by a concurrent caller first; the update below needs it to exist either way.
	_, _ = s.DB.ExecContext(ctx, db.Rebind(s.Dialect, "INSERT INTO "+s.guardTable()+" (model, job_id) VALUES (?, '')"), model)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("facts: begin rebuild replace: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	job, err := s.insertQueued(ctx, tx, contract.NewEventID(), model, Window{})
	if err != nil {
		return nil, err
	}
	err = s.cancelWhole(ctx, tx, model, job.ID)
	if err != nil {
		return nil, err
	}
	guard := "UPDATE " + s.guardTable() + " SET job_id = ? WHERE model = ?"
	if _, err = tx.ExecContext(ctx, db.Rebind(s.Dialect, guard), job.ID, model); err != nil {
		return nil, fmt.Errorf("facts: point backfill guard of %s at its rebuild: %w", model, err)
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("facts: commit rebuild replace: %w", err)
	}
	return job, nil
}

// cancelWhole cancels the queued or running whole-model jobs of model inside tx, except keep.
func (s *JobStore) cancelWhole(ctx context.Context, tx *sql.Tx, model, keep string) error {
	marks, from := inList([]JobStatus{JobQueued, JobRunning})
	query := fmt.Sprintf("UPDATE %s SET status = ?, updated_at = ? WHERE model = ? AND scope_from IS NULL AND id <> ? AND status IN (%s)", s.table(), marks)
	args := append([]any{string(JobCancelled), time.Now().UTC(), model, keep}, from...)
	if _, err := tx.ExecContext(ctx, db.Rebind(s.Dialect, query), args...); err != nil {
		return fmt.Errorf("facts: cancel queued rebuilds of %s: %w", model, err)
	}
	return nil
}

// Progress is how far a model's rebuild has got. Building is true while a job of the model is queued or
// running; Fraction is processed over total across those jobs, 0 until a job has counted its source.
type Progress struct {
	Building bool
	Fraction float64
}

// Progress reports every model in models. A model with no open job is ready, at Fraction 1. The job table
// exists once a tenant's fact models have synced, so Progress runs no DDL: it is read on every query.
func (s *JobStore) Progress(ctx context.Context, models []string) (map[string]Progress, error) {
	out := make(map[string]Progress, len(models))
	for _, m := range models {
		out[m] = Progress{Fraction: 1}
	}
	if len(models) == 0 {
		return out, nil
	}
	modelMarks, args := inList(models)
	statusMarks, statuses := inList([]JobStatus{JobQueued, JobRunning})
	query := fmt.Sprintf("SELECT model, total, processed FROM %s WHERE model IN (%s) AND status IN (%s)", s.table(), modelMarks, statusMarks)
	rows, err := s.DB.QueryContext(ctx, db.Rebind(s.Dialect, query), append(args, statuses...)...)
	if err != nil {
		return nil, fmt.Errorf("facts: read rebuild progress: %w", err)
	}
	defer func() { _ = rows.Close() }()
	sums := map[string][2]int64{}
	for rows.Next() {
		var model string
		var total, processed int64
		if err = rows.Scan(&model, &total, &processed); err != nil {
			return nil, fmt.Errorf("facts: scan rebuild progress: %w", err)
		}
		sum := sums[model]
		sums[model] = [2]int64{sum[0] + total, sum[1] + processed}
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("facts: read rebuild progress: %w", err)
	}
	for model, sum := range sums {
		p := Progress{Building: true}
		if sum[0] > 0 {
			p.Fraction = min(float64(sum[1])/float64(sum[0]), 1)
		}
		out[model] = p
	}
	return out, nil
}

// Cancel moves a queued or running job to cancelled; a running worker stops on its next write.
func (s *JobStore) Cancel(ctx context.Context, id string) error {
	if err := s.ensure(ctx); err != nil {
		return err
	}
	marks, from := inList(sourcesOf(JobCancelled))
	query := fmt.Sprintf("UPDATE %s SET status = ?, updated_at = ? WHERE id = ? AND status IN (%s)", s.table(), marks)
	args := append([]any{string(JobCancelled), time.Now().UTC(), id}, from...)
	changed, err := s.execRows(ctx, query, args)
	if err != nil {
		return fmt.Errorf("facts: cancel rebuild job %s: %w", id, err)
	}
	if changed != 1 {
		return fmt.Errorf("facts: rebuild job %s is neither queued nor running", id)
	}
	return nil
}

// move changes the status of a job the caller holds; extraSet assigns more columns.
func (s *JobStore) move(ctx context.Context, job *Job, to JobStatus, extraSet string, args ...any) error {
	marks, from := inList(sourcesOf(to))
	nowExpr, nowArg := dbClock(s.Dialect, 0)
	query := fmt.Sprintf(`UPDATE %s SET status = ?, %s WHERE id = ? AND status IN (%s) AND lease_owner = ?
		AND fencing_token = ? AND lease_until > %s`, s.table(), extraSet, marks, nowExpr)
	all := append(append([]any{string(to)}, args...), job.ID)
	all = append(append(all, from...), job.owner, job.token, nowArg)
	return s.leased(ctx, query, all, "move")
}

// dbClock is the database clock plus plusSeconds, as SQL and its bound argument. NowMinusSeconds
// subtracts its bound, so the argument is the negation. Leases never use Go's clock.
func dbClock(d db.Dialect, plusSeconds int) (string, any) {
	return d.NowMinusSeconds(), -plusSeconds
}

// leased runs a guarded write and maps a refused write to ErrLeaseLost.
func (s *JobStore) leased(ctx context.Context, query string, args []any, op string) error {
	changed, err := s.execRows(ctx, query, args)
	if err != nil {
		return fmt.Errorf("facts: %s rebuild job: %w", op, err)
	}
	if changed != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *JobStore) execRows(ctx context.Context, query string, args []any) (int64, error) {
	result, err := s.DB.ExecContext(ctx, db.Rebind(s.Dialect, query), args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *JobStore) table() string {
	return s.Dialect.QuoteIdent(jobTable)
}

func (s *JobStore) guardTable() string {
	return s.Dialect.QuoteIdent(guardTable)
}

// execRowsTx runs a write inside tx and returns the rows it changed.
func execRowsTx(ctx context.Context, tx *sql.Tx, query string, args []any) (int64, error) {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ensure creates the job table once per store. DDL runs outside any transaction.
func (s *JobStore) ensure(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready {
		return nil
	}
	if _, err := s.DB.ExecContext(ctx, jobDDL(s.Dialect)); err != nil {
		return fmt.Errorf("facts: create rebuild job table: %w", err)
	}
	if _, err := s.DB.ExecContext(ctx, guardDDL(s.Dialect)); err != nil {
		return fmt.Errorf("facts: create rebuild guard table: %w", err)
	}
	for _, f := range jobColumnsAdded {
		probe := fmt.Sprintf("SELECT %s FROM %s WHERE 1 = 0", s.Dialect.QuoteIdent(f.Fieldname), s.table()) //nolint:gosec // identifiers only, quoted by the dialect
		if _, err := s.DB.ExecContext(ctx, probe); err == nil {
			continue
		}
		if _, err := s.DB.ExecContext(ctx, s.Dialect.AddColumn(jobTable, &f)); err != nil {
			return fmt.Errorf("facts: add rebuild job column %s: %w", f.Fieldname, err)
		}
	}
	s.ready = true
	return nil
}

// jobColumnsAdded are columns added to job tables created before they existed. The scope bounds stay NULL
// for a whole model; total is NOT NULL and defaults to 0 until the job first runs and counts its source.
var jobColumnsAdded = []doctype.Field{
	{Fieldname: "scope_from", Fieldtype: "Data"}, {Fieldname: "scope_to", Fieldtype: "Data"},
	{Fieldname: "total", Fieldtype: "Int", Reqd: true, Default: "0"},
}

// jobDDL creates the job table. IF NOT EXISTS keeps a second process's start harmless.
func jobDDL(dialect db.Dialect) string {
	cols := []string{
		columnDef(dialect, "id", "Data"), columnDef(dialect, "model", "Data"), columnDef(dialect, "status", "Data"),
		columnDef(dialect, "start_sequence", "Int"), columnDef(dialect, "checkpoint", "Data"),
		columnDef(dialect, "processed", "Int"), columnDef(dialect, "total", "Int"), columnDef(dialect, "error", "Text"),
		columnDef(dialect, "lease_owner", "Data"), columnDef(dialect, "fencing_token", "Int"),
		columnDef(dialect, "lease_until", "Datetime"), columnDef(dialect, "created_at", "Datetime"),
		columnDef(dialect, "updated_at", "Datetime"),
		dialect.QuoteIdent("scope_from") + " " + dialect.ColumnType(&doctype.Field{Fieldtype: "Data"}) + " NULL",
		dialect.QuoteIdent("scope_to") + " " + dialect.ColumnType(&doctype.Field{Fieldtype: "Data"}) + " NULL",
		fmt.Sprintf("PRIMARY KEY (%s)", dialect.QuoteIdent("id")),
	}
	ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n  %s\n)", dialect.QuoteIdent(jobTable), strings.Join(cols, ",\n  "))
	if suffix := dialect.TableSuffix(); suffix != "" {
		ddl += " " + suffix
	}
	return ddl
}

// guardDDL creates the backfill guard: one row per model, holding the id of the job that claimed it ("" when
// none has). Its primary key makes a second row for a model impossible.
func guardDDL(dialect db.Dialect) string {
	cols := []string{columnDef(dialect, "model", "Data"), columnDef(dialect, "job_id", "Data"),
		fmt.Sprintf("PRIMARY KEY (%s)", dialect.QuoteIdent("model"))}
	ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n  %s\n)", dialect.QuoteIdent(guardTable), strings.Join(cols, ",\n  "))
	if suffix := dialect.TableSuffix(); suffix != "" {
		ddl += " " + suffix
	}
	return ddl
}

const jobColumns = "id, model, status, start_sequence, checkpoint, processed, total, error, fencing_token, scope_from, scope_to"

// scanJob reads one row selected with jobColumns.
func scanJob(row *sql.Row) (*Job, error) {
	var job Job
	var status string
	var from, to sql.NullString
	if err := row.Scan(&job.ID, &job.Model, &status, &job.StartSequence, &job.Checkpoint, &job.Processed, &job.Total, &job.Error, &job.token, &from, &to); err != nil {
		return nil, err
	}
	job.Status = JobStatus(status)
	var err error
	if from.Valid && from.String != "" {
		if job.Window.From, err = parseWindowDate(from.String); err != nil {
			return nil, err
		}
		job.Window.To, err = parseWindowDate(to.String)
	}
	return &job, err
}

// parseWindowDate reads a stored YYYY-MM-DD bound.
func parseWindowDate(s string) (tenanttime.Date, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return tenanttime.Date{}, fmt.Errorf("facts: parse window date %q: %w", s, err)
	}
	return tenanttime.Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}, nil
}

// readSequence reads the outbox sequence under the dialect's exclusive row lock, so in-flight events finish first.
func readSequence(ctx context.Context, tx *sql.Tx, dialect db.Dialect) (int64, error) {
	var seq int64
	query := "SELECT last_sequence FROM " + sequenceTable + " WHERE id = 1" + dialect.ExclusiveRowLock()
	err := tx.QueryRowContext(ctx, query).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("facts: read outbox sequence: %w", err)
	}
	return seq, nil
}

// inList returns the placeholders and bound values of an IN list.
func inList[T ~string](list []T) (string, []any) {
	marks := make([]string, len(list))
	args := make([]any, len(list))
	for i, v := range list {
		marks[i], args[i] = "?", string(v)
	}
	return strings.Join(marks, ", "), args
}
