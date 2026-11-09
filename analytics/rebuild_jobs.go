package analytics

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

// RebuildRunner processes persisted jobs for one tenant. The Engine runtime
// owns its Run goroutine; HTTP requests only persist work and call Notify.
type RebuildRunner struct {
	db        *sql.DB
	dialect   kdb.Dialect
	site      string
	registry  *doctype.Registry
	owner     string
	admission interface {
		Acquire(context.Context) (func(), error)
	}
	wake               chan struct{}
	running            atomic.Bool
	active             atomic.Int64
	claims             atomic.Int64
	claimFailures      atomic.Int64
	admissionDeferrals atomic.Int64
	executionFailures  atomic.Int64
	leaseRenewFailures atomic.Int64
	leaseLost          atomic.Int64
	completionFailures atomic.Int64
}

// RebuildMetricsSnapshot contains aggregate runner state without tenant or job
// identifiers, suitable for process-level resource telemetry.
type RebuildMetricsSnapshot struct {
	Active, Claims, ClaimFailures, AdmissionDeferrals                    int64
	ExecutionFailures, LeaseRenewFailures, LeaseLost, CompletionFailures int64
}

func (r *RebuildRunner) MetricsSnapshot() RebuildMetricsSnapshot {
	if r == nil {
		return RebuildMetricsSnapshot{}
	}
	return RebuildMetricsSnapshot{
		Active: r.active.Load(), Claims: r.claims.Load(), ClaimFailures: r.claimFailures.Load(),
		AdmissionDeferrals: r.admissionDeferrals.Load(), ExecutionFailures: r.executionFailures.Load(),
		LeaseRenewFailures: r.leaseRenewFailures.Load(), LeaseLost: r.leaseLost.Load(), CompletionFailures: r.completionFailures.Load(),
	}
}

// NewRebuildRunner constructs a bounded, single-worker tenant rebuild runner.
func NewRebuildRunner(database *sql.DB, dialect kdb.Dialect, site string, registry *doctype.Registry, admission interface {
	Acquire(context.Context) (func(), error)
}) *RebuildRunner {
	return &RebuildRunner{db: database, dialect: dialect, site: site, registry: registry, admission: admission, owner: contract.NewEventID(), wake: make(chan struct{}, 1)}
}

// ClaimNextRebuildJob transitions one queued or expired running job to running
// under a database-time lease. Each takeover increments its fencing token.
func ClaimNextRebuildJob(ctx context.Context, database *sql.DB, dialect kdb.Dialect, site, owner string) (*RebuildJob, error) {
	if database == nil {
		return nil, fmt.Errorf("analytics rebuild job requires database")
	}
	if ctx == nil || owner == "" {
		return nil, fmt.Errorf("analytics rebuild claim requires context and owner")
	}
	jobs := dialect.QuoteIdent("_kora_analytics_rebuild_job")
	leases := dialect.QuoteIdent("_kora_analytics_rebuild_lease")
	var job RebuildJob
	var rawFromDate any
	err := database.QueryRowContext(ctx, kdb.Rebind(dialect, fmt.Sprintf(`SELECT j.id, j.doctype, j.from_date FROM %s j
		LEFT JOIN %s l ON l.site = j.site AND l.job_id = j.id
		WHERE j.site = ? AND ((j.status = 'queued' OR j.status = 'running') AND (l.job_id IS NULL OR l.lease_until <= CURRENT_TIMESTAMP))
		ORDER BY j.started_at, j.id LIMIT 1`, jobs, leases)), site).
		Scan(&job.ID, &job.DocType, &rawFromDate)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find queued analytics rebuild: %w", err)
	}
	fromDate, err := parseRebuildDate(rawFromDate)
	if err != nil {
		return nil, fmt.Errorf("parse queued analytics rebuild date: %w", err)
	}
	job.Site, job.From = site, fromDate
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin analytics rebuild claim: %w", err)
	}
	defer tx.Rollback()
	insert := `INSERT INTO %s (site, job_id, owner, fencing_token, lease_until) VALUES (?, ?, ?, 1, %s)`
	leaseDeadline := rebuildLeaseDeadlineExpression(dialect)
	if dialect.DriverName() == "mysql" {
		insert = `INSERT IGNORE INTO %s (site, job_id, owner, fencing_token, lease_until) VALUES (?, ?, ?, 1, %s)`
	} else if dialect.DriverName() == "libsql" {
		insert = `INSERT OR IGNORE INTO %s (site, job_id, owner, fencing_token, lease_until) VALUES (?, ?, ?, 1, %s)`
	} else if dialect.DriverName() == "postgres" {
		insert = `INSERT INTO %s (site, job_id, owner, fencing_token, lease_until) VALUES (?, ?, ?, 1, %s) ON CONFLICT (site, job_id) DO NOTHING`
	}
	result, err := tx.ExecContext(ctx, kdb.Rebind(dialect, fmt.Sprintf(insert, leases, leaseDeadline)), site, job.ID, owner)
	if err != nil {
		return nil, fmt.Errorf("insert analytics rebuild lease: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("check analytics rebuild lease insert: %w", err)
	}
	if changed != 1 {
		update := fmt.Sprintf(`UPDATE %s SET owner = ?, fencing_token = fencing_token + 1, lease_until = %s
			WHERE site = ? AND job_id = ? AND lease_until <= CURRENT_TIMESTAMP`, leases, leaseDeadline)
		result, err = tx.ExecContext(ctx, kdb.Rebind(dialect, update), owner, site, job.ID)
		if err != nil {
			return nil, fmt.Errorf("take over expired analytics rebuild lease: %w", err)
		}
		changed, err = result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("check analytics rebuild lease takeover: %w", err)
		}
		if changed != 1 {
			return nil, nil
		}
	}
	if err := tx.QueryRowContext(ctx, kdb.Rebind(dialect, fmt.Sprintf(`SELECT fencing_token FROM %s WHERE site = ? AND job_id = ? AND owner = ?`, leases)), site, job.ID, owner).Scan(&job.FencingToken); err != nil {
		return nil, fmt.Errorf("read analytics rebuild fencing token: %w", err)
	}
	job.LeaseOwner = owner
	result, err = tx.ExecContext(ctx, kdb.Rebind(dialect, fmt.Sprintf(`UPDATE %s SET status = 'running', error = NULL, updated_at = ?
		WHERE site = ? AND id = ? AND status IN ('queued', 'running')`, jobs)), time.Now().UTC(), site, job.ID)
	if err != nil {
		return nil, fmt.Errorf("mark analytics rebuild running: %w", err)
	}
	changed, err = result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("check analytics rebuild state claim: %w", err)
	}
	if changed != 1 {
		var status string
		if err := tx.QueryRowContext(ctx, kdb.Rebind(dialect, fmt.Sprintf(`SELECT status FROM %s WHERE site = ? AND id = ?`, jobs)), site, job.ID).Scan(&status); err != nil {
			return nil, nil
		}
		if status != "queued" && status != "running" {
			return nil, nil
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit analytics rebuild claim: %w", err)
	}
	return &job, nil
}

func rebuildLeaseDeadlineExpression(dialect kdb.Dialect) string {
	switch dialect.DriverName() {
	case "postgres":
		return "CURRENT_TIMESTAMP + INTERVAL '45 seconds'"
	case "libsql":
		return "datetime(CURRENT_TIMESTAMP, '+45 seconds')"
	default:
		return "DATE_ADD(CURRENT_TIMESTAMP, INTERVAL 45 SECOND)"
	}
}

var ErrRebuildLeaseLost = fmt.Errorf("analytics rebuild lease lost")

func renewRebuildLease(ctx context.Context, database *sql.DB, dialect kdb.Dialect, job *RebuildJob) error {
	leases := dialect.QuoteIdent("_kora_analytics_rebuild_lease")
	query := fmt.Sprintf(`UPDATE %s SET lease_until = %s WHERE site = ? AND job_id = ? AND owner = ? AND fencing_token = ? AND lease_until > CURRENT_TIMESTAMP`, leases, rebuildLeaseDeadlineExpression(dialect))
	result, err := database.ExecContext(ctx, kdb.Rebind(dialect, query), job.Site, job.ID, job.LeaseOwner, job.FencingToken)
	if err != nil {
		return fmt.Errorf("renew analytics rebuild lease: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check analytics rebuild lease renewal: %w", err)
	}
	if changed != 1 {
		return ErrRebuildLeaseLost
	}
	return nil
}

func releaseRebuildLease(ctx context.Context, database *sql.DB, dialect kdb.Dialect, job *RebuildJob) error {
	return transitionClaimedRebuild(ctx, database, dialect, job, `status = 'queued', error = NULL, completed_at = NULL, metrics = 0, updated_at = ?`, time.Now().UTC())
}

func finishClaimedRebuild(ctx context.Context, database *sql.DB, dialect kdb.Dialect, job *RebuildJob, status string, metrics int, failure string) error {
	now := time.Now().UTC()
	set := `status = ?, error = NULL, completed_at = ?, metrics = ?, updated_at = ?`
	args := []any{status, now, metrics, now}
	if status == "failed" {
		set = `status = ?, error = ?, completed_at = ?, updated_at = ?`
		args = []any{status, failure, now, now}
	}
	return transitionClaimedRebuild(ctx, database, dialect, job, set, args...)
}

func transitionClaimedRebuild(ctx context.Context, database *sql.DB, dialect kdb.Dialect, job *RebuildJob, set string, args ...any) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin analytics rebuild state transition: %w", err)
	}
	defer tx.Rollback()
	jobs := dialect.QuoteIdent("_kora_analytics_rebuild_job")
	leases := dialect.QuoteIdent("_kora_analytics_rebuild_lease")
	query := fmt.Sprintf(`UPDATE %s SET %s WHERE site = ? AND id = ? AND status = 'running'
		AND EXISTS (SELECT 1 FROM %s WHERE site = ? AND job_id = ? AND owner = ? AND fencing_token = ? AND lease_until > CURRENT_TIMESTAMP)`, jobs, set, leases)
	args = append(args, job.Site, job.ID, job.Site, job.ID, job.LeaseOwner, job.FencingToken)
	result, err := tx.ExecContext(ctx, kdb.Rebind(dialect, query), args...)
	if err != nil {
		return fmt.Errorf("update analytics rebuild state: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check analytics rebuild state transition: %w", err)
	}
	if changed != 1 {
		return ErrRebuildLeaseLost
	}
	if _, err := tx.ExecContext(ctx, kdb.Rebind(dialect, fmt.Sprintf(`DELETE FROM %s WHERE site = ? AND job_id = ? AND owner = ? AND fencing_token = ?`, leases)), job.Site, job.ID, job.LeaseOwner, job.FencingToken); err != nil {
		return fmt.Errorf("release analytics rebuild lease: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit analytics rebuild state: %w", err)
	}
	return nil
}

// Notify wakes the worker after a durable job insert. Polling remains the
// recovery mechanism for a crash between commit and notification.
func (r *RebuildRunner) Notify() {
	if r == nil {
		return
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run processes one job at a time until its owning tenant runtime is canceled.
func (r *RebuildRunner) Run(ctx context.Context) {
	if r == nil || r.db == nil || r.registry == nil || ctx == nil || !r.running.CompareAndSwap(false, true) {
		return
	}
	defer r.running.Store(false)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		job, err := r.claim(ctx)
		if err != nil {
			if ctx.Err() == nil {
				r.claimFailures.Add(1)
				slog.Error("claim analytics rebuild job failed", "site", r.site, "error", err)
			}
		} else if job != nil {
			r.claims.Add(1)
			r.process(ctx, job)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-ticker.C:
		}
	}
}

func (r *RebuildRunner) claim(ctx context.Context) (*RebuildJob, error) {
	if r.admission == nil {
		return ClaimNextRebuildJob(ctx, r.db, r.dialect, r.site, r.owner)
	}
	release, err := r.admission.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("admit analytics rebuild claim: %w", err)
	}
	defer release()
	return ClaimNextRebuildJob(ctx, r.db, r.dialect, r.site, r.owner)
}

func (r *RebuildRunner) process(ctx context.Context, job *RebuildJob) {
	if r.admission != nil {
		acquire := r.admission.Acquire
		if maintenance, ok := r.admission.(interface {
			AcquireMaintenance(context.Context) (func(), error)
		}); ok {
			acquire = maintenance.AcquireMaintenance
		}
		release, err := acquire(ctx)
		if err != nil {
			r.admissionDeferrals.Add(1)
			// Do not bypass endpoint admission to release the claim. Its short
			// database-time lease expires and makes the durable job claimable
			// again without issuing unadmitted SQL while the lane is saturated.
			slog.Warn("analytics rebuild deferred by work admission; claim will expire", "site", r.site, "job_id", job.ID, "error", err)
			if ctx.Err() == nil {
				timer := time.NewTimer(time.Second)
				select {
				case <-ctx.Done():
				case <-timer.C:
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			return
		}
		defer release()
	}
	r.active.Add(1)
	defer r.active.Add(-1)
	workCtx, cancelWork := context.WithCancel(ctx)
	heartbeatDone := make(chan error, 1)
	stopHeartbeat := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				heartbeatDone <- nil
				return
			case <-stopHeartbeat:
				heartbeatDone <- nil
				return
			case <-ticker.C:
				heartbeatCtx, heartbeatCancel := context.WithTimeout(workCtx, 5*time.Second)
				err := renewRebuildLease(heartbeatCtx, r.db, r.dialect, job)
				heartbeatCancel()
				if err != nil {
					cancelWork()
					heartbeatDone <- err
					return
				}
			}
		}
	}()
	count, err := BackfillContext(workCtx, r.db, r.dialect, r.site, r.registry, job.From, job.DocType)
	close(stopHeartbeat)
	heartbeatErr := <-heartbeatDone
	cancelWork()
	if ctx.Err() != nil {
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer releaseCancel()
		if stateErr := releaseRebuildLease(releaseCtx, r.db, r.dialect, job); stateErr != nil && stateErr != ErrRebuildLeaseLost {
			r.completionFailures.Add(1)
			slog.Error("requeue canceled analytics rebuild failed", "site", r.site, "job_id", job.ID, "error", stateErr)
		} else if stateErr == ErrRebuildLeaseLost {
			r.leaseLost.Add(1)
		}
		return
	}
	if heartbeatErr != nil {
		r.leaseRenewFailures.Add(1)
		if heartbeatErr == ErrRebuildLeaseLost {
			r.leaseLost.Add(1)
		}
		if heartbeatErr != ErrRebuildLeaseLost {
			slog.Error("analytics rebuild lease heartbeat failed", "site", r.site, "job_id", job.ID, "error", heartbeatErr)
		}
		return
	}
	if err != nil {
		r.executionFailures.Add(1)
		finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer finishCancel()
		if stateErr := finishClaimedRebuild(finishCtx, r.db, r.dialect, job, "failed", 0, err.Error()); stateErr != nil && stateErr != ErrRebuildLeaseLost {
			r.completionFailures.Add(1)
			slog.Error("mark analytics rebuild failed failed", "site", r.site, "job_id", job.ID, "rebuild_error", err, "state_error", stateErr)
		} else if stateErr == ErrRebuildLeaseLost {
			r.leaseLost.Add(1)
		}
		return
	}
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer finishCancel()
	if err := finishClaimedRebuild(finishCtx, r.db, r.dialect, job, "completed", count, ""); err != nil {
		if err == ErrRebuildLeaseLost {
			r.leaseLost.Add(1)
		} else {
			r.completionFailures.Add(1)
		}
		if err != ErrRebuildLeaseLost {
			slog.Error("mark analytics rebuild completed failed", "site", r.site, "job_id", job.ID, "error", err)
		}
	}
}

// RebuildJob is the durable state of an administrator-triggered analytics rebuild.
type RebuildJob struct {
	ID           string     `json:"id"`
	Site         string     `json:"-"`
	LeaseOwner   string     `json:"-"`
	FencingToken int64      `json:"-"`
	DocType      string     `json:"doctype,omitempty"`
	From         time.Time  `json:"-"`
	Status       string     `json:"status"`
	StartedAt    time.Time  `json:"started_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	Metrics      int        `json:"metrics,omitempty"`
	Error        string     `json:"error,omitempty"`
}

func CreateRebuildJob(db *sql.DB, dialect kdb.Dialect, job *RebuildJob) error {
	if db == nil || job == nil {
		return fmt.Errorf("analytics rebuild job requires database and job")
	}
	table := dialect.QuoteIdent("_kora_analytics_rebuild_job")
	_, err := db.Exec(kdb.Rebind(dialect, fmt.Sprintf(`INSERT INTO %s
		(id, site, doctype, from_date, status, started_at, metrics, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, table)),
		job.ID, job.Site, job.DocType, job.From.Format("2006-01-02"), job.Status,
		job.StartedAt, job.Metrics, job.StartedAt)
	return err
}

func GetRebuildJob(db *sql.DB, dialect kdb.Dialect, site, id string) (*RebuildJob, error) {
	if db == nil {
		return nil, fmt.Errorf("analytics rebuild job requires database")
	}
	table := dialect.QuoteIdent("_kora_analytics_rebuild_job")
	var job RebuildJob
	var rawFromDate any
	var completed sql.NullTime
	err := db.QueryRow(kdb.Rebind(dialect, fmt.Sprintf(`SELECT id, site, doctype, from_date, status, started_at,
		completed_at, metrics, COALESCE(error, '') FROM %s WHERE site = ? AND id = ?`, table)), site, id).
		Scan(&job.ID, &job.Site, &job.DocType, &rawFromDate, &job.Status, &job.StartedAt,
			&completed, &job.Metrics, &job.Error)
	if err != nil {
		return nil, err
	}
	job.From, err = parseRebuildDate(rawFromDate)
	if err != nil {
		return nil, fmt.Errorf("parse analytics rebuild date: %w", err)
	}
	if completed.Valid {
		job.CompletedAt = &completed.Time
	}
	return &job, nil
}

func parseRebuildDate(value any) (time.Time, error) {
	switch date := value.(type) {
	case time.Time:
		return time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC), nil
	case string:
		parsed, err := time.Parse("2006-01-02", date)
		if err != nil {
			return time.Time{}, err
		}
		return parsed, nil
	case []byte:
		parsed, err := time.Parse("2006-01-02", string(date))
		if err != nil {
			return time.Time{}, err
		}
		return parsed, nil
	default:
		return time.Time{}, fmt.Errorf("unsupported SQL date value %T", value)
	}
}
