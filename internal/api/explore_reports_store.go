package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/metrics"
)

// savedReport is one row of _kora_saved_report. BrokenAt is zero while the report runs.
type savedReport struct {
	ID           string
	Name         string
	Visibility   string
	Owner        string
	Question     contract.SavedQuestion
	RecordTypes  []string
	BrokenReason string
	BrokenAt     time.Time
	Created      time.Time
	Modified     time.Time
}

// contract returns the wire form of the report.
func (rep savedReport) contract() contract.SavedReport {
	out := contract.SavedReport{ID: rep.ID, Name: rep.Name, Visibility: rep.Visibility, Owner: rep.Owner,
		Question: rep.Question, RecordTypes: rep.RecordTypes, BrokenReason: rep.BrokenReason,
		Modified: rep.Modified.UTC().Format(time.RFC3339)}
	if !rep.BrokenAt.IsZero() {
		out.BrokenAt = rep.BrokenAt.UTC().Format(time.RFC3339)
	}
	return out
}

// recheck returns rep with the outcome of its question against cat. A question that still checks keeps its
// record types; one that does not is broken with the reason, and keeps the moment it first broke.
func (rep savedReport) recheck(cat metrics.Catalog, now time.Time) savedReport {
	models, err := checkSavedQuestion(cat, rep.Question)
	if err != nil {
		if rep.BrokenReason != err.Error() || rep.BrokenAt.IsZero() {
			rep.BrokenAt = now
		}
		rep.BrokenReason = err.Error()
		return rep
	}
	rep.RecordTypes = models
	rep.BrokenReason = ""
	rep.BrokenAt = time.Time{}
	return rep
}

// reportStore reads and writes the saved reports of one site database.
type reportStore struct {
	database *sql.DB
	dialect  db.Dialect
}

const savedReportColumns = `id, name, visibility, owner, question, record_types, broken_reason, broken_at, created, modified`

// list returns every saved report of the site, by name. Visibility is applied by the caller.
func (s reportStore) list(ctx context.Context) ([]savedReport, error) {
	rows, err := s.database.QueryContext(ctx, `SELECT `+savedReportColumns+` FROM _kora_saved_report ORDER BY name_key`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // read-only query; a close error after a full read changes nothing
	var out []savedReport
	for rows.Next() {
		rep, err := scanSavedReport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rep)
	}
	return out, rows.Err()
}

// get returns the report with id. A missing report is sql.ErrNoRows.
func (s reportStore) get(ctx context.Context, id string) (savedReport, error) {
	row := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT `+savedReportColumns+` FROM _kora_saved_report WHERE id = ?`), id)
	return scanSavedReport(row)
}

// nameTaken reports whether another report than exceptID already has the name, ignoring case and the ends.
func (s reportStore) nameTaken(ctx context.Context, name, exceptID string) (bool, error) {
	var n int
	err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT COUNT(*) FROM _kora_saved_report WHERE name_key = ? AND id <> ?`), reportNameKey(name), exceptID).Scan(&n)
	return n > 0, err
}

// insert adds a new report.
func (s reportStore) insert(ctx context.Context, rep savedReport) error {
	question, recordTypes, err := encodeReport(rep)
	if err != nil {
		return err
	}
	_, err = s.database.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_saved_report
		(id, name, name_key, visibility, owner, question, record_types, broken_reason, broken_at, created, modified)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		rep.ID, rep.Name, reportNameKey(rep.Name), rep.Visibility, rep.Owner, question, recordTypes,
		rep.BrokenReason, nullTime(rep.BrokenAt), rep.Created, rep.Modified)
	return err
}

// update replaces the name, visibility, question and state of a report.
func (s reportStore) update(ctx context.Context, rep savedReport) error {
	question, recordTypes, err := encodeReport(rep)
	if err != nil {
		return err
	}
	_, err = s.database.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_saved_report
		SET name = ?, name_key = ?, visibility = ?, question = ?, record_types = ?, broken_reason = ?, broken_at = ?, modified = ?
		WHERE id = ?`),
		rep.Name, reportNameKey(rep.Name), rep.Visibility, question, recordTypes, rep.BrokenReason, nullTime(rep.BrokenAt), rep.Modified, rep.ID)
	return err
}

// setState writes only the broken state and the record types of a report, after a recheck.
func (s reportStore) setState(ctx context.Context, rep savedReport) error {
	_, recordTypes, err := encodeReport(rep)
	if err != nil {
		return err
	}
	_, err = s.database.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_saved_report SET record_types = ?, broken_reason = ?, broken_at = ? WHERE id = ?`),
		recordTypes, rep.BrokenReason, nullTime(rep.BrokenAt), rep.ID)
	return err
}

// remove deletes a report.
func (s reportStore) remove(ctx context.Context, id string) error {
	_, err := s.database.ExecContext(ctx, db.Rebind(s.dialect, `DELETE FROM _kora_saved_report WHERE id = ?`), id)
	return err
}

// scanSavedReport reads one row in savedReportColumns order.
func scanSavedReport(row interface{ Scan(dest ...any) error }) (savedReport, error) {
	var rep savedReport
	var question, recordTypes string
	var brokenAt sql.NullTime
	if err := row.Scan(&rep.ID, &rep.Name, &rep.Visibility, &rep.Owner, &question, &recordTypes, &rep.BrokenReason, &brokenAt, &rep.Created, &rep.Modified); err != nil {
		return savedReport{}, err
	}
	if err := json.Unmarshal([]byte(question), &rep.Question); err != nil {
		return savedReport{}, err
	}
	if err := json.Unmarshal([]byte(recordTypes), &rep.RecordTypes); err != nil {
		return savedReport{}, err
	}
	if brokenAt.Valid {
		rep.BrokenAt = brokenAt.Time
	}
	return rep, nil
}

// encodeReport returns the JSON text of the question and of the record types, never null.
func encodeReport(rep savedReport) (string, string, error) {
	question, err := json.Marshal(rep.Question)
	if err != nil {
		return "", "", err
	}
	types := rep.RecordTypes
	if types == nil {
		types = []string{}
	}
	recordTypes, err := json.Marshal(types)
	if err != nil {
		return "", "", err
	}
	return string(question), string(recordTypes), nil
}

// reportNameKey is the name a unique report name is compared by: lower case, ends trimmed.
func reportNameKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// nullTime is t, or NULL when t is zero.
func nullTime(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: !t.IsZero()}
}
