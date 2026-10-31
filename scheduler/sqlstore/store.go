// Package sqlstore persists scheduled definitions and their occurrences.
package sqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/scheduler"
)

const maxJobJSONBytes = 1 << 20

type Store struct {
	database *sql.DB
	dialect  db.Dialect
	ensureMu sync.Mutex
	ensured  bool
}

func New(database *sql.DB, dialect db.Dialect) (*Store, error) {
	if database == nil || dialect == nil {
		return nil, errors.New("scheduled job SQL store requires database and dialect")
	}
	switch dialect.DriverName() {
	case "mysql", "postgres", "libsql":
	default:
		return nil, fmt.Errorf("unsupported scheduler database dialect %q", dialect.DriverName())
	}
	return &Store{database: database, dialect: dialect}, nil
}

func (s *Store) Ensure(ctx context.Context) error {
	if s == nil || s.database == nil || s.dialect == nil {
		return errors.New("scheduled job SQL store is not configured")
	}
	s.ensureMu.Lock()
	defer s.ensureMu.Unlock()
	if s.ensured {
		return nil
	}
	for _, statement := range tableStatements(s.dialect.DriverName()) {
		if _, err := s.database.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("ensure scheduled job table: %w", err)
		}
	}
	if s.dialect.DriverName() == "mysql" {
		var indexes int
		if err := s.database.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='_kora_schedule_occurrence' AND index_name='idx_schedule_pending_backlog'`).Scan(&indexes); err != nil {
			return fmt.Errorf("inspect scheduled backlog index: %w", err)
		}
		if indexes == 0 {
			if _, err := s.database.ExecContext(ctx, `CREATE INDEX idx_schedule_pending_backlog ON _kora_schedule_occurrence (tenant_id, state, scheduled_at)`); err != nil {
				return fmt.Errorf("create scheduled backlog index: %w", err)
			}
		}
	}
	s.ensured = true
	return nil
}

// BacklogMetrics returns the count and oldest scheduled time of materialized
// pending occurrences for one tenant. The composite due index bounds the scan
// to that tenant's pending schedules; SQL work is admitted by the scheduler.
func (s *Store) BacklogMetrics(ctx context.Context, tenant string) (int64, time.Time, error) {
	if ctx == nil || tenant == "" {
		return 0, time.Time{}, errors.New("scheduled backlog metrics require context and tenant")
	}
	var count int64
	var oldest nullableScheduleTime
	err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect,
		`SELECT COUNT(*), MIN(scheduled_at) FROM _kora_schedule_occurrence WHERE tenant_id=? AND state='pending'`), tenant).Scan(&count, &oldest)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("read pending scheduled occurrence backlog: %w", err)
	}
	if !oldest.Valid {
		return count, time.Time{}, nil
	}
	return count, oldest.Time.UTC(), nil
}

// NextRunAt returns the persisted cursor for one scheduled definition. It is a
// wake-up hint only; ClaimDue remains authoritative about whether work is due.
func (s *Store) NextRunAt(ctx context.Context, tenant, jobName string) (time.Time, error) {
	if ctx == nil || tenant == "" || jobName == "" {
		return time.Time{}, errors.New("scheduled next run requires context, tenant and job name")
	}
	var next db.Timestamp
	if err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect,
		`SELECT next_run_at FROM _kora_schedule_definition WHERE tenant_id=? AND job_name=?`), tenant, jobName).Scan(&next); err != nil {
		return time.Time{}, fmt.Errorf("read scheduled definition next run: %w", err)
	}
	return next.Time.UTC(), nil
}

type nullableScheduleTime struct {
	Time  time.Time
	Valid bool
}

func (value *nullableScheduleTime) Scan(source any) error {
	if source == nil {
		value.Time = time.Time{}
		value.Valid = false
		return nil
	}
	var timestamp db.Timestamp
	if err := timestamp.Scan(source); err != nil {
		return err
	}
	value.Time = timestamp.UTC()
	value.Valid = true
	return nil
}

func (s *Store) RegisterDefinition(ctx context.Context, tenant string, job *scheduler.JobConfig, digest string, firstRun, now time.Time) error {
	if err := validateIdentity(tenant, job); err != nil {
		return err
	}
	body, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("encode scheduled job: %w", err)
	}
	if len(body) > maxJobJSONBytes {
		return fmt.Errorf("scheduled job JSON exceeds %d bytes", maxJobJSONBytes)
	}
	if digest == "" || digest != scheduler.JobDefinitionDigest(job) {
		return errors.New("scheduled job definition digest mismatch")
	}
	stampNow, stampFirst := encodeTime(s.dialect.DriverName(), now), encodeTime(s.dialect.DriverName(), firstRun)
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schedule registration: %w", err)
	}
	defer tx.Rollback()
	query := `SELECT definition_digest FROM _kora_schedule_definition WHERE tenant_id=? AND job_name=?`
	if s.dialect.DriverName() == "mysql" || s.dialect.DriverName() == "postgres" {
		query += " FOR UPDATE"
	}
	var existing string
	err = tx.QueryRowContext(ctx, db.Rebind(s.dialect, query), tenant, job.Name).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		insert := `INSERT INTO _kora_schedule_definition (tenant_id,job_name,definition_digest,schedule,job_json,next_run_at,updated_at) VALUES (?,?,?,?,?,?,?)`
		switch s.dialect.DriverName() {
		case "mysql":
			insert = `INSERT IGNORE INTO _kora_schedule_definition (tenant_id,job_name,definition_digest,schedule,job_json,next_run_at,updated_at) VALUES (?,?,?,?,?,?,?)`
		default:
			insert += ` ON CONFLICT (tenant_id,job_name) DO NOTHING`
		}
		if _, err = tx.ExecContext(ctx, db.Rebind(s.dialect, insert), tenant, job.Name, digest, job.Schedule, string(body), stampFirst, stampNow); err != nil {
			return fmt.Errorf("insert scheduled definition: %w", err)
		}
		err = tx.QueryRowContext(ctx, db.Rebind(s.dialect, query), tenant, job.Name).Scan(&existing)
	}
	if err != nil {
		return fmt.Errorf("read scheduled definition: %w", err)
	}
	if existing != digest {
		_, err = tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_schedule_definition SET definition_digest=?,schedule=?,job_json=?,next_run_at=?,updated_at=? WHERE tenant_id=? AND job_name=?`), digest, job.Schedule, string(body), stampFirst, stampNow, tenant, job.Name)
	}
	if err != nil {
		return fmt.Errorf("write scheduled definition: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit scheduled definition: %w", err)
	}
	return nil
}

func (s *Store) ClaimDue(ctx context.Context, tenant, jobName, owner string, now time.Time, lease time.Duration) (*scheduler.ScheduleOccurrence, error) {
	if tenant == "" || jobName == "" || owner == "" || lease <= 0 {
		return nil, errors.New("invalid scheduled occurrence claim")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin scheduled occurrence claim: %w", err)
	}
	defer tx.Rollback()
	if err := s.markExpiredRunningUnknown(ctx, tx, tenant, jobName); err != nil {
		return nil, err
	}
	var unknown int
	if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT COUNT(*) FROM _kora_schedule_occurrence WHERE tenant_id=? AND job_name=? AND state='outcome_unknown'`), tenant, jobName).Scan(&unknown); err != nil {
		return nil, fmt.Errorf("check uncertain scheduled occurrences: %w", err)
	}
	if unknown > 0 {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit uncertain schedule observation: %w", err)
		}
		return nil, scheduler.ErrScheduleOutcomeUnknown
	}
	var running int
	if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT COUNT(*) FROM _kora_schedule_occurrence WHERE tenant_id=? AND job_name=? AND state='running' AND lease_until>`+s.dialect.NowTimestamp()), tenant, jobName).Scan(&running); err != nil {
		return nil, fmt.Errorf("check running scheduled occurrence: %w", err)
	}
	if running > 0 {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit active schedule observation: %w", err)
		}
		return nil, nil
	}
	occurrence, err := s.pendingOccurrence(ctx, tx, tenant, jobName)
	if errors.Is(err, sql.ErrNoRows) {
		occurrence, err = s.materializeDue(ctx, tx, tenant, jobName, now)
	}
	if err != nil {
		return nil, err
	}
	if occurrence == nil {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit empty schedule claim: %w", err)
		}
		return nil, nil
	}
	deadline := leaseDeadlineExpression(s.dialect.DriverName())
	result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_schedule_occurrence SET state='running',owner=?,fence=fence+1,lease_until=`+deadline+`,attempts=attempts+1 WHERE tenant_id=? AND job_name=? AND scheduled_at=? AND state='pending'`), owner, lease.Microseconds(), tenant, jobName, encodeTime(s.dialect.DriverName(), occurrence.ScheduledAt))
	if err != nil {
		return nil, fmt.Errorf("claim scheduled occurrence: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("read schedule claim result: %w", err)
	}
	if changed != 1 {
		return nil, fmt.Errorf("scheduled occurrence claim changed %d rows", changed)
	}
	occurrence.Owner = owner
	occurrence.Fence++
	if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT fence,attempts FROM _kora_schedule_occurrence WHERE tenant_id=? AND job_name=? AND scheduled_at=?`), tenant, jobName, encodeTime(s.dialect.DriverName(), occurrence.ScheduledAt)).Scan(&occurrence.Fence, &occurrence.Attempts); err != nil {
		return nil, fmt.Errorf("read scheduled occurrence fence: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit scheduled occurrence claim: %w", err)
	}
	return occurrence, nil
}

func (s *Store) Renew(ctx context.Context, occurrence scheduler.ScheduleOccurrence, now time.Time, lease time.Duration) error {
	result, err := s.database.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_schedule_occurrence SET lease_until=`+leaseDeadlineExpression(s.dialect.DriverName())+` WHERE tenant_id=? AND job_name=? AND scheduled_at=? AND state='running' AND owner=? AND fence=? AND lease_until>`+s.dialect.NowTimestamp()), lease.Microseconds(), occurrence.TenantID, occurrence.JobName, encodeTime(s.dialect.DriverName(), occurrence.ScheduledAt), occurrence.Owner, occurrence.Fence)
	if err != nil {
		return fmt.Errorf("renew scheduled occurrence lease: %w", err)
	}
	return requireOne(result, scheduler.ErrScheduleLeaseLost)
}

func (s *Store) Complete(ctx context.Context, occurrence scheduler.ScheduleOccurrence, nextRun, now time.Time) error {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schedule completion: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_schedule_occurrence SET state='completed',lease_until=NULL,finished_at=`+s.dialect.NowTimestamp()+`,last_error='' WHERE tenant_id=? AND job_name=? AND scheduled_at=? AND state='running' AND owner=? AND fence=? AND lease_until>`+s.dialect.NowTimestamp()), occurrence.TenantID, occurrence.JobName, encodeTime(s.dialect.DriverName(), occurrence.ScheduledAt), occurrence.Owner, occurrence.Fence)
	if err != nil {
		return fmt.Errorf("complete scheduled occurrence: %w", err)
	}
	if err := requireOne(result, scheduler.ErrScheduleLeaseLost); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_schedule_definition SET next_run_at=?,updated_at=? WHERE tenant_id=? AND job_name=? AND definition_digest=?`), encodeTime(s.dialect.DriverName(), nextRun), encodeTime(s.dialect.DriverName(), now), occurrence.TenantID, occurrence.JobName, occurrence.DefinitionDigest); err != nil {
		return fmt.Errorf("advance scheduled definition cursor: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schedule completion: %w", err)
	}
	return nil
}

func (s *Store) MarkOutcomeUnknown(ctx context.Context, occurrence scheduler.ScheduleOccurrence, now time.Time, reason string) error {
	if len(reason) > 4096 {
		reason = reason[:4096]
	}
	result, err := s.database.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_schedule_occurrence SET state='outcome_unknown',lease_until=NULL,finished_at=`+s.dialect.NowTimestamp()+`,last_error=? WHERE tenant_id=? AND job_name=? AND scheduled_at=? AND state='running' AND owner=? AND fence=?`), reason, occurrence.TenantID, occurrence.JobName, encodeTime(s.dialect.DriverName(), occurrence.ScheduledAt), occurrence.Owner, occurrence.Fence)
	if err != nil {
		return fmt.Errorf("mark scheduled occurrence outcome unknown: %w", err)
	}
	return requireOne(result, scheduler.ErrScheduleLeaseLost)
}

func (s *Store) markExpiredRunningUnknown(ctx context.Context, tx *sql.Tx, tenant, jobName string) error {
	_, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_schedule_occurrence SET state='outcome_unknown',lease_until=NULL,finished_at=`+s.dialect.NowTimestamp()+`,last_error='lease expired while execution outcome was unknown' WHERE tenant_id=? AND job_name=? AND state='running' AND lease_until<=`+s.dialect.NowTimestamp()), tenant, jobName)
	if err != nil {
		return fmt.Errorf("fence expired scheduled occurrences: %w", err)
	}
	return nil
}

func (s *Store) pendingOccurrence(ctx context.Context, tx *sql.Tx, tenant, jobName string) (*scheduler.ScheduleOccurrence, error) {
	query := `SELECT scheduled_at,definition_digest,job_json FROM _kora_schedule_occurrence WHERE tenant_id=? AND job_name=? AND state='pending' ORDER BY scheduled_at LIMIT 1`
	if s.dialect.DriverName() == "mysql" || s.dialect.DriverName() == "postgres" {
		query += " FOR UPDATE"
	}
	var scheduled db.Timestamp
	var digest, body string
	if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, query), tenant, jobName).Scan(&scheduled, &digest, &body); err != nil {
		return nil, err
	}
	return decodeOccurrence(tenant, jobName, scheduled.Time, digest, body)
}

func (s *Store) materializeDue(ctx context.Context, tx *sql.Tx, tenant, jobName string, now time.Time) (*scheduler.ScheduleOccurrence, error) {
	query := `SELECT definition_digest,schedule,job_json,next_run_at FROM _kora_schedule_definition WHERE tenant_id=? AND job_name=? AND next_run_at<=?`
	if s.dialect.DriverName() == "mysql" || s.dialect.DriverName() == "postgres" {
		query += " FOR UPDATE SKIP LOCKED"
	}
	var digest, schedule, body string
	var scheduled db.Timestamp
	if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, query), tenant, jobName, encodeTime(s.dialect.DriverName(), now)).Scan(&digest, &schedule, &body, &scheduled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read due scheduled definition: %w", err)
	}
	next := scheduler.NextRunAfter(schedule, scheduled.Time)
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_schedule_occurrence (tenant_id,job_name,scheduled_at,definition_digest,job_json,state,attempts,fence,last_error) VALUES (?,?,?,?,?,'pending',0,0,'')`), tenant, jobName, encodeTime(s.dialect.DriverName(), scheduled.Time), digest, body); err != nil {
		return nil, fmt.Errorf("persist due scheduled occurrence: %w", err)
	}
	result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_schedule_definition SET next_run_at=? WHERE tenant_id=? AND job_name=? AND definition_digest=? AND next_run_at=?`), encodeTime(s.dialect.DriverName(), next), tenant, jobName, digest, encodeTime(s.dialect.DriverName(), scheduled.Time))
	if err != nil {
		return nil, fmt.Errorf("advance due schedule cursor: %w", err)
	}
	if err := requireOne(result, scheduler.ErrScheduleLeaseLost); err != nil {
		return nil, err
	}
	return decodeOccurrence(tenant, jobName, scheduled.Time, digest, body)
}

func decodeOccurrence(tenant, jobName string, scheduled time.Time, digest, body string) (*scheduler.ScheduleOccurrence, error) {
	var job scheduler.JobConfig
	if err := json.Unmarshal([]byte(body), &job); err != nil {
		return nil, fmt.Errorf("decode scheduled occurrence: %w", err)
	}
	key := tenant + "\x00" + jobName + "\x00" + scheduled.UTC().Format(time.RFC3339Nano)
	idDigest := sha256.Sum256([]byte(key))
	return &scheduler.ScheduleOccurrence{TenantID: tenant, JobName: jobName, ID: hex.EncodeToString(idDigest[:]), ScheduledAt: scheduled, DefinitionDigest: digest, Job: job}, nil
}

func validateIdentity(tenant string, job *scheduler.JobConfig) error {
	if tenant == "" || job == nil || job.Name == "" || job.Schedule == "" {
		return errors.New("tenant, job name and schedule are required")
	}
	if len(tenant) > 140 || len(job.Name) > 140 {
		return errors.New("scheduled job identity exceeds storage limit")
	}
	return nil
}

func encodeTime(driver string, value time.Time) any {
	value = value.UTC()
	if driver == "libsql" {
		return value.Format("2006-01-02 15:04:05.000")
	}
	return value
}

func leaseDeadlineExpression(driver string) string {
	switch driver {
	case "mysql":
		return "DATE_ADD(CURRENT_TIMESTAMP(6), INTERVAL ? MICROSECOND)"
	case "postgres":
		return "(NOW() + (? * INTERVAL '1 microsecond'))"
	case "libsql":
		return `STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW', '+' || (? / 1000000.0) || ' seconds')`
	default:
		panic("unsupported scheduler lease dialect: " + driver)
	}
}

func requireOne(result sql.Result, failure error) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read scheduled row update result: %w", err)
	}
	if count != 1 {
		return failure
	}
	return nil
}
