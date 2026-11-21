package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/metrics"
)

// savedReport is one row of _kora_saved_report. Broken is nil while the report runs, and BrokenAt is zero
// then. OwnerName is not stored: the store fills it for the response.
type savedReport struct {
	ID          string
	Name        string
	Visibility  string
	Owner       string
	OwnerName   string
	Question    contract.SavedQuestion
	RecordTypes []string
	Broken      *contract.SavedReportBroken
	BrokenAt    time.Time
	Created     time.Time
	Modified    time.Time
}

// contract returns the wire form of the report.
func (rep savedReport) contract() contract.SavedReport {
	out := contract.SavedReport{ID: rep.ID, Name: rep.Name, Visibility: rep.Visibility, Owner: rep.Owner,
		OwnerName: rep.OwnerName, Question: rep.Question, RecordTypes: rep.RecordTypes, Broken: rep.Broken,
		Modified: rep.Modified.UTC().Format(time.RFC3339)}
	if !rep.BrokenAt.IsZero() {
		out.BrokenAt = rep.BrokenAt.UTC().Format(time.RFC3339)
	}
	return out
}

// recheck returns rep with the outcome of its question against cat. A question that still checks keeps its
// record types; one that does not is broken with the piece it lost, and keeps the moment it first broke.
func (rep savedReport) recheck(cat metrics.Catalog, join map[string]bool, now time.Time) savedReport {
	models, err := checkSavedQuestion(cat, join, rep.Question)
	if err != nil {
		broken := brokenOf(err)
		if rep.Broken == nil || *rep.Broken != *broken || rep.BrokenAt.IsZero() {
			rep.BrokenAt = now
		}
		rep.Broken = broken
		return rep
	}
	rep.RecordTypes = models
	rep.Broken = nil
	rep.BrokenAt = time.Time{}
	return rep
}

// reportStore reads and writes the saved reports of one site database.
type reportStore struct {
	database *sql.DB
	dialect  db.Dialect
}

const savedReportColumns = `id, name, visibility, owner, question, record_types, broken_reason, broken_at, created, modified`

// ownerNames returns the full name of each owner of site that has one, keyed by user id. An owner without a
// name is absent, and the caller shows the user id.
func (s reportStore) ownerNames(ctx context.Context, site string, owners []string) (map[string]string, error) {
	names := map[string]string{}
	if len(owners) == 0 {
		return names, nil
	}
	args := []any{site}
	marks := make([]string, 0, len(owners))
	for _, owner := range owners {
		args = append(args, owner)
		marks = append(marks, "?")
	}
	rows, err := s.database.QueryContext(ctx, db.Rebind(s.dialect, `SELECT name, full_name FROM _kora_user WHERE site = ? AND name IN (`+strings.Join(marks, ", ")+`)`), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // read-only query; a close error after a full read changes nothing
	for rows.Next() {
		var id, fullName string
		if err := rows.Scan(&id, &fullName); err != nil {
			return nil, err
		}
		if fullName != "" {
			names[id] = fullName
		}
	}
	return names, rows.Err()
}

// fillOwnerNames sets OwnerName on each report: the owner's full name, or the owner's user id.
func (s reportStore) fillOwnerNames(ctx context.Context, site string, reps []savedReport) error {
	owners := make([]string, 0, len(reps))
	for _, rep := range reps {
		owners = append(owners, rep.Owner)
	}
	names, err := s.ownerNames(ctx, site, slices.Compact(slices.Sorted(slices.Values(owners))))
	if err != nil {
		return err
	}
	for i := range reps {
		reps[i].OwnerName = reps[i].Owner
		if name, ok := names[reps[i].Owner]; ok {
			reps[i].OwnerName = name
		}
	}
	return nil
}

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

// errReportNameTaken is returned by insert and update when the unique name index refuses the row, which a
// concurrent save of the same name can cause after the name check passed.
var errReportNameTaken = errors.New("saved report name is taken")

// writeErr returns errReportNameTaken for a unique violation and err otherwise.
func writeErr(err error) error {
	if db.IsUniqueViolation(err) {
		return errReportNameTaken
	}
	return err
}

// insert adds a new report.
func (s reportStore) insert(ctx context.Context, rep savedReport) error {
	question, recordTypes, broken, err := encodeReport(rep)
	if err != nil {
		return err
	}
	_, err = s.database.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_saved_report
		(id, name, name_key, visibility, owner, question, record_types, broken_reason, broken_at, created, modified)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		rep.ID, rep.Name, reportNameKey(rep.Name), rep.Visibility, rep.Owner, question, recordTypes,
		broken, nullTime(rep.BrokenAt), rep.Created, rep.Modified)
	return writeErr(err)
}

// update replaces the name, visibility, question and state of a report.
func (s reportStore) update(ctx context.Context, rep savedReport) error {
	question, recordTypes, broken, err := encodeReport(rep)
	if err != nil {
		return err
	}
	_, err = s.database.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_saved_report
		SET name = ?, name_key = ?, visibility = ?, question = ?, record_types = ?, broken_reason = ?, broken_at = ?, modified = ?
		WHERE id = ?`),
		rep.Name, reportNameKey(rep.Name), rep.Visibility, question, recordTypes, broken, nullTime(rep.BrokenAt), rep.Modified, rep.ID)
	return writeErr(err)
}

// setState writes only the broken state and the record types of a report, after a recheck.
func (s reportStore) setState(ctx context.Context, rep savedReport) error {
	_, recordTypes, broken, err := encodeReport(rep)
	if err != nil {
		return err
	}
	_, err = s.database.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_saved_report SET record_types = ?, broken_reason = ?, broken_at = ? WHERE id = ?`),
		recordTypes, broken, nullTime(rep.BrokenAt), rep.ID)
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
	var question, recordTypes, broken string
	var brokenAt sql.NullTime
	if err := row.Scan(&rep.ID, &rep.Name, &rep.Visibility, &rep.Owner, &question, &recordTypes, &broken, &brokenAt, &rep.Created, &rep.Modified); err != nil {
		return savedReport{}, err
	}
	if err := json.Unmarshal([]byte(question), &rep.Question); err != nil {
		return savedReport{}, err
	}
	if err := json.Unmarshal([]byte(recordTypes), &rep.RecordTypes); err != nil {
		return savedReport{}, err
	}
	rep.Broken = decodeBroken(broken)
	if brokenAt.Valid {
		rep.BrokenAt = brokenAt.Time
	}
	return rep, nil
}

// decodeBroken reads the broken column: JSON of the broken piece, or empty while the report runs. Text that is
// not JSON is an invalid question labelled with the text, so a stored row always loads.
func decodeBroken(stored string) *contract.SavedReportBroken {
	if stored == "" {
		return nil
	}
	var b contract.SavedReportBroken
	if err := json.Unmarshal([]byte(stored), &b); err != nil || b.Kind == "" {
		return &contract.SavedReportBroken{Kind: brokenInvalidQuestion, Label: stored}
	}
	return &b
}

// encodeBroken returns the broken column text for rep: JSON of the broken piece, or empty.
func encodeBroken(b *contract.SavedReportBroken) (string, error) {
	if b == nil {
		return "", nil
	}
	out, err := json.Marshal(b)
	return string(out), err
}

// encodeReport returns the JSON text of the question and of the record types, never null, and the broken
// column text.
func encodeReport(rep savedReport) (question, recordTypes, broken string, err error) {
	q, err := json.Marshal(rep.Question)
	if err != nil {
		return "", "", "", err
	}
	types := rep.RecordTypes
	if types == nil {
		types = []string{}
	}
	t, err := json.Marshal(types)
	if err != nil {
		return "", "", "", err
	}
	broken, err = encodeBroken(rep.Broken)
	if err != nil {
		return "", "", "", err
	}
	return string(q), string(t), broken, nil
}

// reportNameKey is the name a unique report name is compared by: lower case, ends trimmed.
func reportNameKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// nullTime is t, or NULL when t is zero.
func nullTime(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: !t.IsZero()}
}
