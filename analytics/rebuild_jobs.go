package analytics

import (
	"database/sql"
	"fmt"
	"time"

	kdb "github.com/asenawritescode/kora/db"
)

// RebuildJob is the durable state of an administrator-triggered analytics rebuild.
type RebuildJob struct {
	ID          string     `json:"id"`
	Site        string     `json:"-"`
	DocType     string     `json:"doctype,omitempty"`
	From        time.Time  `json:"-"`
	Status      string     `json:"status"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Metrics     int        `json:"metrics,omitempty"`
	Error       string     `json:"error,omitempty"`
}

func CreateRebuildJob(db *sql.DB, dialect kdb.SchemaDialect, job *RebuildJob) error {
	if db == nil || job == nil {
		return fmt.Errorf("analytics rebuild job requires database and job")
	}
	table := dialect.QuoteIdent("_kora_analytics_rebuild_job")
	_, err := db.Exec(fmt.Sprintf(`INSERT INTO %s
		(id, site, doctype, from_date, status, started_at, metrics, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, table),
		job.ID, job.Site, job.DocType, job.From.Format("2006-01-02"), job.Status,
		job.StartedAt, job.Metrics, job.StartedAt)
	return err
}

func GetRebuildJob(db *sql.DB, dialect kdb.SchemaDialect, site, id string) (*RebuildJob, error) {
	if db == nil {
		return nil, fmt.Errorf("analytics rebuild job requires database")
	}
	table := dialect.QuoteIdent("_kora_analytics_rebuild_job")
	var job RebuildJob
	var fromDate time.Time
	var completed sql.NullTime
	err := db.QueryRow(fmt.Sprintf(`SELECT id, site, doctype, from_date, status, started_at,
		completed_at, metrics, COALESCE(error, '') FROM %s WHERE site = ? AND id = ?`, table), site, id).
		Scan(&job.ID, &job.Site, &job.DocType, &fromDate, &job.Status, &job.StartedAt,
			&completed, &job.Metrics, &job.Error)
	if err != nil {
		return nil, err
	}
	job.From = fromDate
	if completed.Valid {
		job.CompletedAt = &completed.Time
	}
	return &job, nil
}

func MarkRebuildRunning(db *sql.DB, dialect kdb.SchemaDialect, site, id string) error {
	return updateRebuildJob(db, dialect, site, id,
		`status = ?, error = NULL, updated_at = ?`, "running", time.Now().UTC())
}

func MarkRebuildCompleted(db *sql.DB, dialect kdb.SchemaDialect, site, id string, metrics int) error {
	now := time.Now().UTC()
	return updateRebuildJob(db, dialect, site, id,
		`status = ?, metrics = ?, completed_at = ?, updated_at = ?`, "completed", metrics, now, now)
}

func MarkRebuildFailed(db *sql.DB, dialect kdb.SchemaDialect, site, id string, cause error) error {
	now := time.Now().UTC()
	message := "rebuild failed"
	if cause != nil {
		message = cause.Error()
	}
	return updateRebuildJob(db, dialect, site, id,
		`status = ?, error = ?, completed_at = ?, updated_at = ?`, "failed", message, now, now)
}

func updateRebuildJob(db *sql.DB, dialect kdb.SchemaDialect, site, id, set string, args ...any) error {
	if db == nil {
		return fmt.Errorf("analytics rebuild job requires database")
	}
	table := dialect.QuoteIdent("_kora_analytics_rebuild_job")
	args = append(args, site, id)
	_, err := db.Exec(fmt.Sprintf(`UPDATE %s SET %s WHERE site = ? AND id = ?`, table, set), args...)
	return err
}
