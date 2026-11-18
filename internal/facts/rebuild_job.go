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
)

const jobTable = "_kora_fact_rebuild_job"

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

// Job is one rebuild job; owner and token are the lease a Claim took.
type Job struct {
	ID            string
	Model         string
	Status        JobStatus
	StartSequence int64 // newest outbox sequence the rebuild covers; fixed at enqueue
	Checkpoint    string
	Processed     int64
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
func (s *JobStore) Enqueue(ctx context.Context, model string) (*Job, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("facts: begin rebuild enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	start, err := readSequence(ctx, tx)
	if err != nil {
		return nil, err
	}
	job := &Job{ID: contract.NewEventID(), Model: model, Status: JobQueued, StartSequence: start}
	now := time.Now().UTC()
	insert := fmt.Sprintf(`INSERT INTO %s (id, model, status, start_sequence, checkpoint, processed, error, lease_owner, fencing_token, lease_until, created_at, updated_at)
		VALUES (?, ?, ?, ?, '', 0, '', '', 0, %s, ?, ?)`, s.table(), s.Dialect.NowTimestamp())
	if _, err = tx.ExecContext(ctx, db.Rebind(s.Dialect, insert), job.ID, model, string(job.Status), start, now, now); err != nil {
		return nil, fmt.Errorf("facts: insert rebuild job: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("facts: commit rebuild enqueue: %w", err)
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
	s.ready = true
	return nil
}

// jobDDL creates the job table. IF NOT EXISTS keeps a second process's start harmless.
func jobDDL(dialect db.Dialect) string {
	cols := []string{
		columnDef(dialect, "id", "Data"), columnDef(dialect, "model", "Data"), columnDef(dialect, "status", "Data"),
		columnDef(dialect, "start_sequence", "Int"), columnDef(dialect, "checkpoint", "Data"),
		columnDef(dialect, "processed", "Int"), columnDef(dialect, "error", "Text"),
		columnDef(dialect, "lease_owner", "Data"), columnDef(dialect, "fencing_token", "Int"),
		columnDef(dialect, "lease_until", "Datetime"), columnDef(dialect, "created_at", "Datetime"),
		columnDef(dialect, "updated_at", "Datetime"),
		fmt.Sprintf("PRIMARY KEY (%s)", dialect.QuoteIdent("id")),
	}
	ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n  %s\n)", dialect.QuoteIdent(jobTable), strings.Join(cols, ",\n  "))
	if suffix := dialect.TableSuffix(); suffix != "" {
		ddl += " " + suffix
	}
	return ddl
}

const jobColumns = "id, model, status, start_sequence, checkpoint, processed, error, fencing_token"

// scanJob reads one row selected with jobColumns.
func scanJob(row *sql.Row) (*Job, error) {
	var job Job
	var status string
	if err := row.Scan(&job.ID, &job.Model, &status, &job.StartSequence, &job.Checkpoint, &job.Processed, &job.Error, &job.token); err != nil {
		return nil, err
	}
	job.Status = JobStatus(status)
	return &job, nil
}

func readSequence(ctx context.Context, tx *sql.Tx) (int64, error) {
	var seq int64
	err := tx.QueryRowContext(ctx, "SELECT last_sequence FROM "+sequenceTable+" WHERE id = 1 FOR UPDATE").Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("facts: read outbox sequence: %w", err)
	}
	return seq, nil
}

// inList returns the placeholders and bound values of an IN list of statuses.
func inList(list []JobStatus) (string, []any) {
	marks := make([]string, len(list))
	args := make([]any, len(list))
	for i, s := range list {
		marks[i], args[i] = "?", string(s)
	}
	return strings.Join(marks, ", "), args
}
