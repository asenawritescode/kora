package site

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sqlDialect "github.com/asenawritescode/kora/internal/db"
)

type OnboardingStore struct {
	DB      *sql.DB
	dialect sqlDialect.Dialect
	mu      sync.Mutex
	path    string
}

func NewOnboardingStore(db *sql.DB, dbType ...string) *OnboardingStore {
	typeName := ""
	if len(dbType) > 0 {
		typeName = dbType[0]
	}
	if typeName == "" {
		typeName = os.Getenv("KORA_DB_TYPE")
	}
	path := os.Getenv("KORA_CONFIG_DIR")
	if path == "" {
		path = "."
	}
	return &OnboardingStore{DB: db, dialect: sqlDialect.Resolve(typeName), path: filepath.Join(path, "_kora_provisioning_jobs.json")}
}

func (s *OnboardingStore) Bootstrap() error {
	if s.DB == nil {
		return nil
	}
	timestampType := "DATETIME"
	booleanType := "TINYINT(1)"
	booleanDefault := "0"
	if _, ok := s.dialect.(*sqlDialect.PostgresDialect); ok {
		timestampType = "TIMESTAMP"
		booleanType = "BOOLEAN"
		booleanDefault = "FALSE"
	} else if _, ok := s.dialect.(*sqlDialect.LibSQLDialect); ok {
		booleanType = "INTEGER"
	}
	uniqueConstraint := "UNIQUE KEY uq_kora_provisioning_dedupe (site, resource, operation_id, dedupe_hash)"
	if _, ok := s.dialect.(*sqlDialect.MySQLDialect); !ok {
		uniqueConstraint = "CONSTRAINT uq_kora_provisioning_dedupe UNIQUE (site, resource, operation_id, dedupe_hash)"
	}
	_, err := s.DB.Exec(fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS _kora_provisioning_job (
  id VARCHAR(140) NOT NULL,
  site VARCHAR(140) NOT NULL DEFAULT '',
  resource VARCHAR(140) NOT NULL DEFAULT '',
  state VARCHAR(40) NOT NULL DEFAULT '',
  operation_id VARCHAR(140) NOT NULL DEFAULT '',
  idempotency_key VARCHAR(255) NOT NULL DEFAULT '',
  input_fingerprint VARCHAR(255) NOT NULL DEFAULT '',
  dedupe_hash VARCHAR(64) NOT NULL DEFAULT '',
  provider_request_id VARCHAR(255) NOT NULL DEFAULT '',
  output_id VARCHAR(255) NOT NULL DEFAULT '',
  attempt INT NOT NULL DEFAULT 0,
  lease_owner VARCHAR(140) NOT NULL DEFAULT '',
  lease_until %s,
  retry_at %s,
  last_error TEXT,
  created_at %s NOT NULL,
  updated_at %s,
  PRIMARY KEY (id),
  %s
)
`, timestampType, timestampType, timestampType, timestampType, uniqueConstraint))
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`ALTER TABLE _kora_provisioning_job ADD COLUMN dedupe_hash VARCHAR(64) NOT NULL DEFAULT ''`)
	if err != nil && !strings.Contains(err.Error(), "Duplicate column name") && !strings.Contains(err.Error(), "duplicate column") && !strings.Contains(err.Error(), "already exists") {
		return err
	}
	_, err = s.DB.Exec(fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS _kora_provisioning_checkpoint (
  job_id VARCHAR(140) NOT NULL,
  stage VARCHAR(140) NOT NULL,
  completed %s NOT NULL DEFAULT %s,
  recorded_at %s NOT NULL,
  PRIMARY KEY (job_id, stage)
)
`, booleanType, booleanDefault, timestampType))
	return err
}

func (s *OnboardingStore) UpsertJob(job OnboardingJob) error {
	if s.DB == nil {
		return s.upsertFile(job)
	}
	query := `
INSERT INTO _kora_provisioning_job (
  id, site, resource, state, operation_id, idempotency_key, input_fingerprint, dedupe_hash, provider_request_id, output_id,
  attempt, lease_owner, lease_until, retry_at, last_error, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`
	query += "\n" + s.dialect.UpsertClause(
		[]string{"site", "resource", "operation_id", "dedupe_hash"},
		[]string{"site", "resource", "state", "operation_id", "idempotency_key", "input_fingerprint", "dedupe_hash", "provider_request_id", "output_id", "attempt", "lease_owner", "lease_until", "retry_at", "last_error", "updated_at"},
	)
	_, err := s.DB.Exec(sqlDialect.Rebind(s.dialect, query),
		job.ID, job.Site, job.Resource, string(job.State), job.OperationID, job.IdempotencyKey, job.InputFingerprint, dedupeHash(job.Site, job.Resource, job.OperationID, job.IdempotencyKey, job.InputFingerprint), job.ProviderRequestID, job.OutputID,
		job.Attempt, job.LeaseOwner, nullableTime(job.LeaseUntil), nullableTime(job.RetryAt), job.LastError, job.CreatedAt, nullableTime(job.UpdatedAt),
	)
	return err
}

func (s *OnboardingStore) UpsertCheckpoint(cp OnboardingCheckpoint) error {
	if s.DB == nil {
		return s.upsertCheckpointFile(cp)
	}
	query := `
INSERT INTO _kora_provisioning_checkpoint (job_id, stage, completed, recorded_at)
VALUES (?, ?, ?, ?)
`
	query += "\n" + s.dialect.UpsertClause([]string{"job_id", "stage"}, []string{"completed", "recorded_at"})
	completed := any(boolToInt(cp.Completed))
	if _, ok := s.dialect.(*sqlDialect.PostgresDialect); ok {
		completed = cp.Completed
	}
	_, err := s.DB.Exec(sqlDialect.Rebind(s.dialect, query), cp.JobID, cp.Stage, completed, cp.RecordedAt)
	return err
}

func (s *OnboardingStore) LoadJob(site, resource, operationID, idempotencyKey, fingerprint string) (*OnboardingJob, error) {
	if s.DB == nil {
		return s.loadJobFile(site, resource, operationID, idempotencyKey, fingerprint)
	}
	row := s.DB.QueryRow(sqlDialect.Rebind(s.dialect, `
SELECT id, site, resource, state, operation_id, idempotency_key, input_fingerprint, provider_request_id, output_id,
       attempt, lease_owner, lease_until, retry_at, last_error, created_at, updated_at
FROM _kora_provisioning_job
WHERE site = ? AND resource = ? AND operation_id = ? AND dedupe_hash = ?
ORDER BY created_at DESC LIMIT 1
`), site, resource, operationID, dedupeHash(site, resource, operationID, idempotencyKey, fingerprint))
	var job OnboardingJob
	var leaseUntil, retryAt, updatedAt sql.NullTime
	var leaseOwner, providerReq, outputID, lastError sql.NullString
	if err := row.Scan(&job.ID, &job.Site, &job.Resource, &job.State, &job.OperationID, &job.IdempotencyKey, &job.InputFingerprint, &providerReq, &outputID, &job.Attempt, &leaseOwner, &leaseUntil, &retryAt, &lastError, &job.CreatedAt, &updatedAt); err != nil {
		return nil, err
	}
	job.ProviderRequestID = providerReq.String
	job.OutputID = outputID.String
	job.LeaseOwner = leaseOwner.String
	job.LastError = lastError.String
	if leaseUntil.Valid {
		job.LeaseUntil = leaseUntil.Time
	}
	if retryAt.Valid {
		job.RetryAt = retryAt.Time
	}
	if updatedAt.Valid {
		job.UpdatedAt = updatedAt.Time
	}
	return &job, nil
}

func (s *OnboardingStore) LoadJobByID(id string) (*OnboardingJob, error) {
	if s.DB == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		state, err := s.readFileState()
		if err != nil {
			return nil, err
		}
		for i := len(state.Jobs) - 1; i >= 0; i-- {
			job := state.Jobs[i]
			if job.ID == id {
				return &job, nil
			}
		}
		return nil, sql.ErrNoRows
	}
	row := s.DB.QueryRow(sqlDialect.Rebind(s.dialect, `
SELECT id, site, resource, state, operation_id, idempotency_key, input_fingerprint, provider_request_id, output_id,
       attempt, lease_owner, lease_until, retry_at, last_error, created_at, updated_at
FROM _kora_provisioning_job
WHERE id = ?
`), id)
	var job OnboardingJob
	var leaseUntil, retryAt, updatedAt sql.NullTime
	var leaseOwner, providerReq, outputID, lastError sql.NullString
	if err := row.Scan(&job.ID, &job.Site, &job.Resource, &job.State, &job.OperationID, &job.IdempotencyKey, &job.InputFingerprint, &providerReq, &outputID, &job.Attempt, &leaseOwner, &leaseUntil, &retryAt, &lastError, &job.CreatedAt, &updatedAt); err != nil {
		return nil, err
	}
	job.ProviderRequestID = providerReq.String
	job.OutputID = outputID.String
	job.LeaseOwner = leaseOwner.String
	job.LastError = lastError.String
	if leaseUntil.Valid {
		job.LeaseUntil = leaseUntil.Time
	}
	if retryAt.Valid {
		job.RetryAt = retryAt.Time
	}
	if updatedAt.Valid {
		job.UpdatedAt = updatedAt.Time
	}
	return &job, nil
}

func (s *OnboardingStore) ListJobs() ([]OnboardingJob, error) {
	if s.DB == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		state, err := s.readFileState()
		if err != nil {
			return nil, err
		}
		jobs := make([]OnboardingJob, len(state.Jobs))
		copy(jobs, state.Jobs)
		return jobs, nil
	}
	rows, err := s.DB.Query(`
SELECT id, site, resource, state, operation_id, idempotency_key, input_fingerprint, provider_request_id, output_id,
       attempt, lease_owner, lease_until, retry_at, last_error, created_at, updated_at
FROM _kora_provisioning_job
ORDER BY created_at DESC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []OnboardingJob
	for rows.Next() {
		var job OnboardingJob
		var leaseUntil, retryAt, updatedAt sql.NullTime
		var leaseOwner, providerReq, outputID, lastError sql.NullString
		if err := rows.Scan(&job.ID, &job.Site, &job.Resource, &job.State, &job.OperationID, &job.IdempotencyKey, &job.InputFingerprint, &providerReq, &outputID, &job.Attempt, &leaseOwner, &leaseUntil, &retryAt, &lastError, &job.CreatedAt, &updatedAt); err != nil {
			return nil, err
		}
		job.ProviderRequestID = providerReq.String
		job.OutputID = outputID.String
		job.LeaseOwner = leaseOwner.String
		job.LastError = lastError.String
		if leaseUntil.Valid {
			job.LeaseUntil = leaseUntil.Time
		}
		if retryAt.Valid {
			job.RetryAt = retryAt.Time
		}
		if updatedAt.Valid {
			job.UpdatedAt = updatedAt.Time
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func (s *OnboardingStore) EncodeCheckpoint(cp OnboardingCheckpoint) string {
	b, _ := json.Marshal(cp)
	return string(b)
}

type onboardingFileState struct {
	Jobs        []OnboardingJob        `json:"jobs"`
	Checkpoints []OnboardingCheckpoint `json:"checkpoints"`
}

func (s *OnboardingStore) readFileState() (onboardingFileState, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return onboardingFileState{}, nil
		}
		return onboardingFileState{}, err
	}
	var state onboardingFileState
	if err := json.Unmarshal(b, &state); err != nil {
		return onboardingFileState{}, err
	}
	return state, nil
}

func (s *OnboardingStore) writeFileState(state onboardingFileState) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomically(s.path, b, 0o600)
}

func writeFileAtomically(path string, data []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".kora-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary state file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set temporary state file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary state file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publish state file: %w", err)
	}
	if d, openErr := os.Open(dir); openErr == nil {
		defer func() {
			if closeErr := d.Close(); err == nil && closeErr != nil {
				err = fmt.Errorf("close state directory: %w", closeErr)
			}
		}()
		if syncErr := d.Sync(); syncErr != nil {
			return fmt.Errorf("sync state directory: %w", syncErr)
		}
	} else {
		return fmt.Errorf("open state directory for sync: %w", openErr)
	}
	return nil
}

func (s *OnboardingStore) upsertFile(job OnboardingJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readFileState()
	if err != nil {
		return err
	}
	replaced := false
	for i := range state.Jobs {
		if state.Jobs[i].Site == job.Site && state.Jobs[i].Resource == job.Resource && state.Jobs[i].OperationID == job.OperationID && state.Jobs[i].IdempotencyKey == job.IdempotencyKey && state.Jobs[i].InputFingerprint == job.InputFingerprint {
			state.Jobs[i] = job
			replaced = true
			break
		}
	}
	if !replaced {
		state.Jobs = append(state.Jobs, job)
	}
	return s.writeFileState(state)
}

func (s *OnboardingStore) upsertCheckpointFile(cp OnboardingCheckpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readFileState()
	if err != nil {
		return err
	}
	replaced := false
	for i := range state.Checkpoints {
		if state.Checkpoints[i].JobID == cp.JobID && state.Checkpoints[i].Stage == cp.Stage {
			state.Checkpoints[i] = cp
			replaced = true
			break
		}
	}
	if !replaced {
		state.Checkpoints = append(state.Checkpoints, cp)
	}
	return s.writeFileState(state)
}

func (s *OnboardingStore) loadJobFile(site, resource, operationID, idempotencyKey, fingerprint string) (*OnboardingJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readFileState()
	if err != nil {
		return nil, err
	}
	for i := len(state.Jobs) - 1; i >= 0; i-- {
		job := state.Jobs[i]
		if job.Site == site && job.Resource == resource && job.OperationID == operationID && job.IdempotencyKey == idempotencyKey && job.InputFingerprint == fingerprint {
			return &job, nil
		}
	}
	return nil, sql.ErrNoRows
}

func dedupeHash(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(strings.TrimSpace(part)))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
