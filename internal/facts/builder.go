package facts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/eventstream"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// ConsumerName is the transactional outbox consumer that writes fact rows.
const ConsumerName = "analytics-facts-v1"

// dirtyTable marks (model, business_date) partitions whose summaries must be recomputed.
const dirtyTable = "_kora_fact_dirty"

// Builder applies record change events to fact tables. It is a transactional outbox
// consumer: the fact rows, the dirty markers and the consumer receipt commit together.
// Delivery state (receipts, quarantine) belongs to the outbox Dispatcher, not to this type.
type Builder struct {
	DB       *sql.DB // DDL runs here, not in the consumer transaction (MySQL DDL commits implicitly)
	Dialect  db.Dialect
	Registry *doctype.Registry

	mu    sync.Mutex
	ready map[*doctype.DocType]bool // per doctype definition: a config revision is a new pointer
}

// EventTypes lists the record event types of every doctype with a fact model.
func EventTypes(reg *doctype.Registry) []string {
	if reg == nil {
		return nil
	}
	ops := []eventstream.EventOp{eventstream.EventInsert, eventstream.EventUpdate, eventstream.EventDelete, eventstream.EventSubmit, eventstream.EventCancel}
	var types []string
	for _, dt := range reg.All() {
		if dt.Facts == nil {
			continue
		}
		for _, op := range ops {
			types = append(types, outbox.EventTypeName(dt.Name, op))
		}
	}
	return types
}

// recordPayload is the outbox payload written by the orm for a record change.
type recordPayload struct {
	Data     map[string]any `json:"data"`
	OldData  map[string]any `json:"old_data"`
	Table    string         `json:"table"`
	RecordID string         `json:"record_id"`
}

// Consume is the outbox TransactionalWorker. A record without a business date is an error,
// so the dispatcher quarantines the event instead of storing a row without a day.
func (b *Builder) Consume(ctx context.Context, tx *sql.Tx, event contract.EventEnvelope) error {
	dt, change, err := b.decode(event)
	if err != nil {
		return err
	}
	err = b.ensure(ctx, dt)
	if err != nil {
		return err
	}
	clock, err := tenanttime.Load(ctx, tx, b.Dialect, event.Site)
	if err != nil {
		return fmt.Errorf("facts: load tenant clock: %w", err)
	}
	if change.Owner, change.Created, err = b.author(ctx, tx, dt, change); err != nil {
		return err
	}
	head, lines := Map(dt, change, clock)
	if head.BusinessDate == (tenanttime.Date{}) {
		return fmt.Errorf("facts: %s %s has no business date on axis %q", dt.Name, change.RecordID, dt.Facts.Time)
	}
	if err := b.writeRows(ctx, tx, dt, head, lines); err != nil {
		return err
	}
	// Backdating moves a record between partitions: mark the day it left as well as the day it reached.
	return b.markDirtyDates(ctx, tx, dt.Name, []tenanttime.Date{head.BusinessDate, leftDate(dt, change, clock)})
}

// writeRows stores the header row and its line rows.
func (b *Builder) writeRows(ctx context.Context, tx *sql.Tx, dt *doctype.DocType, head Row, lines []Row) error {
	if err := b.writeRow(ctx, tx, dt.Facts.TableName(dt), head); err != nil {
		return err
	}
	for _, line := range lines {
		if err := b.writeRow(ctx, tx, dt.Facts.LineTableName(dt), line); err != nil {
			return err
		}
	}
	return nil
}

// leftDate is the day a record left when this change moved it, or the zero Date.
func leftDate(dt *doctype.DocType, change Change, clock tenanttime.Clock) tenanttime.Date {
	if change.OldData == nil || change.Deleted {
		return tenanttime.Date{}
	}
	old, _ := Map(dt, Change{RecordID: change.RecordID, Created: change.Created, Deleted: true, OldData: change.OldData}, clock)
	return old.BusinessDate
}

// markDirtyDates marks each non-zero partition day for recomputation.
func (b *Builder) markDirtyDates(ctx context.Context, tx *sql.Tx, model string, dates []tenanttime.Date) error {
	for _, d := range dates {
		if d == (tenanttime.Date{}) {
			continue
		}
		if err := b.markDirty(ctx, tx, model, d); err != nil {
			return err
		}
	}
	return nil
}

// decode reads the record payload and the event's sequence and scope.
func (b *Builder) decode(event contract.EventEnvelope) (*doctype.DocType, Change, error) {
	var p recordPayload
	if err := json.Unmarshal(event.Data, &p); err != nil {
		return nil, Change{}, fmt.Errorf("facts: decode record event: %w", err)
	}
	dt := b.Registry.Get(p.Table)
	if dt == nil || dt.Facts == nil {
		return nil, Change{}, fmt.Errorf("facts: no fact model for doctype %q", p.Table)
	}
	seq, err := strconv.ParseInt(string(event.Sequence), 10, 64)
	if err != nil {
		return nil, Change{}, fmt.Errorf("facts: parse source sequence: %w", err)
	}
	return dt, Change{
		RecordID:    p.RecordID,
		LegalEntity: event.BusinessScope.LegalEntityID,
		Sequence:    seq,
		OccurredAt:  event.OccurredAt,
		Deleted:     strings.HasSuffix(event.Type, ".after_delete"),
		Data:        p.Data,
		OldData:     p.OldData,
	}, nil
}

// author reads owner and creation from the record table in tx. A missing record (deleted, or
// deleted after this event) falls back to the payload, which carries the full row.
func (b *Builder) author(ctx context.Context, tx *sql.Tx, dt *doctype.DocType, change Change) (string, time.Time, error) {
	var owner, created any
	query := fmt.Sprintf("SELECT owner, creation FROM %s WHERE name = ?", b.Dialect.QuoteIdent(dt.RawTableName()))
	err := tx.QueryRowContext(ctx, db.Rebind(b.Dialect, query), change.RecordID).Scan(&owner, &created)
	if errors.Is(err, sql.ErrNoRows) {
		src := change.Data
		if src == nil {
			src = change.OldData
		}
		owner, created = src["owner"], src["creation"]
	} else if err != nil {
		return "", time.Time{}, fmt.Errorf("facts: read record author: %w", err)
	}
	return text(owner), instant(created), nil
}

// writeRow inserts row, or overwrites the stored row when its sequence is not newer.
// A newer stored row wins, so a late or redelivered event never rolls a fact back.
func (b *Builder) writeRow(ctx context.Context, tx *sql.Tx, table string, row Row) error {
	names, args := rowColumns(row)
	var stored int64
	query := fmt.Sprintf("SELECT source_sequence FROM %s WHERE record_id = ? AND line_id = ? FOR UPDATE", b.Dialect.QuoteIdent(table))
	err := tx.QueryRowContext(ctx, db.Rebind(b.Dialect, query), row.RecordID, row.LineID).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return b.exec(ctx, tx, insertSQL(b.Dialect, table, names), args)
	case err != nil:
		return fmt.Errorf("facts: read stored row: %w", err)
	case stored > row.SourceSequence:
		return nil
	}
	sets := make([]string, len(names))
	for i, name := range names {
		sets[i] = b.Dialect.QuoteIdent(name) + " = ?"
	}
	update := fmt.Sprintf("UPDATE %s SET %s WHERE record_id = ? AND line_id = ?", b.Dialect.QuoteIdent(table), strings.Join(sets, ", "))
	return b.exec(ctx, tx, update, append(args, row.RecordID, row.LineID))
}

// markDirty records that a partition needs recomputing. One upsert: a first mark inserts version 1,
// and each later mark bumps version, the value a refresher's fence checks.
func (b *Builder) markDirty(ctx context.Context, tx *sql.Tx, model string, d tenanttime.Date) error {
	inc, ok := b.Dialect.(db.TableQualifiedIncrementDialect)
	if !ok {
		return errors.New("facts: dialect has no table-qualified increment")
	}
	upsert := fmt.Sprintf("INSERT INTO %s (model, business_date, version) VALUES (?, ?, 1) %s",
		b.Dialect.QuoteIdent(dirtyTable), inc.UpsertIncrementForTable(dirtyTable, []string{"model", "business_date"}, []string{"version"}))
	return b.exec(ctx, tx, upsert, []any{model, d.String()})
}

// exec runs one statement with ? placeholders, rebound for the dialect.
func (b *Builder) exec(ctx context.Context, tx *sql.Tx, query string, args []any) error {
	if _, err := tx.ExecContext(ctx, db.Rebind(b.Dialect, query), args...); err != nil {
		return fmt.Errorf("facts: write fact row: %w", err)
	}
	return nil
}

// ensure creates the dirty table and dt's fact tables when missing. It runs once per
// doctype definition per process; a missing table is probed, not inferred from an error.
func (b *Builder) ensure(ctx context.Context, dt *doctype.DocType) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ready[dt] {
		return nil
	}
	if err := b.execDDL(ctx, []string{dirtyDDL(b.Dialect), summaryDDL(b.Dialect, dt)}); err != nil {
		return err
	}
	if err := b.migrateDirty(ctx); err != nil {
		return err
	}
	var stmts []string
	switch {
	case !b.tableExists(ctx, dt.Facts.TableName(dt)):
		stmts = DDL(b.Dialect, dt)
	case dt.Facts.Lines != "" && !b.tableExists(ctx, dt.Facts.LineTableName(dt)):
		stmts = createTable(b.Dialect, dt.Facts.LineTableName(dt), dt.Facts.Columns())
	}
	if err := b.execDDL(ctx, stmts); err != nil {
		return err
	}
	if b.ready == nil {
		b.ready = make(map[*doctype.DocType]bool)
	}
	b.ready[dt] = true
	return nil
}

func (b *Builder) execDDL(ctx context.Context, stmts []string) error {
	for _, stmt := range stmts {
		if _, err := b.DB.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("facts: create table: %w", err)
		}
	}
	return nil
}

func (b *Builder) tableExists(ctx context.Context, table string) bool {
	_, err := b.DB.ExecContext(ctx, fmt.Sprintf("SELECT 1 FROM %s WHERE 1 = 0", b.Dialect.QuoteIdent(table)))
	return err == nil
}

// dirtyDDL creates the dirty-partition table with its key. Columns added later are in dirtyColumns.
func dirtyDDL(dialect db.Dialect) string {
	ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n  %s,\n  %s,\n  PRIMARY KEY (%s, %s)\n)",
		dialect.QuoteIdent(dirtyTable), columnDef(dialect, "model", "Data"), columnDef(dialect, "business_date", "Date"),
		dialect.QuoteIdent("model"), dialect.QuoteIdent("business_date"))
	if suffix := dialect.TableSuffix(); suffix != "" {
		ddl += " " + suffix
	}
	return ddl
}

// dirtyColumns are added with the dialect's AddColumn when an existing table lacks them.
var dirtyColumns = []doctype.Field{{Fieldname: "version", Fieldtype: "Int", Reqd: true, Default: "0"}, {Fieldname: "lease_owner", Fieldtype: "Data"}, {Fieldname: "lease_at", Fieldtype: "Datetime"}}

// migrateDirty adds each dirty column the table lacks. The probe is dialect-neutral; LoadSchema skips _kora_ tables.
func (b *Builder) migrateDirty(ctx context.Context) error {
	for _, f := range dirtyColumns {
		probe := fmt.Sprintf("SELECT %s FROM %s WHERE 1 = 0", b.Dialect.QuoteIdent(f.Fieldname), b.Dialect.QuoteIdent(dirtyTable)) //nolint:gosec // identifiers only, quoted by the dialect
		if _, err := b.DB.ExecContext(ctx, probe); err == nil {
			continue
		}
		if err := b.execDDL(ctx, []string{b.Dialect.AddColumn(dirtyTable, &f)}); err != nil {
			return err
		}
	}
	return nil
}

// rowColumns returns every column of row with its value, sorted by column name.
func rowColumns(row Row) ([]string, []any) {
	values := map[string]any{
		"record_id": row.RecordID, "line_id": row.LineID, "legal_entity": row.LegalEntity, "owner": row.Owner,
		"business_date": row.BusinessDate.String(), "occurred_at": row.OccurredAt, "status": row.Status,
		"source_sequence": row.SourceSequence, "deleted": row.Deleted,
	}
	for k, v := range row.Measures {
		values[k] = v.String()
	}
	for k, v := range row.Dimensions {
		values[k] = v
	}
	names := make([]string, 0, len(values))
	for k := range values {
		names = append(names, k)
	}
	sort.Strings(names)
	args := make([]any, len(names))
	for i, name := range names {
		args[i] = values[name]
	}
	return names, args
}

// insertSQL builds a plain INSERT with one placeholder per column.
func insertSQL(dialect db.Dialect, table string, names []string) string {
	cols := make([]string, len(names))
	marks := make([]string, len(names))
	for i, name := range names {
		cols[i] = dialect.QuoteIdent(name)
		marks[i] = "?"
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", dialect.QuoteIdent(table), strings.Join(cols, ", "), strings.Join(marks, ", "))
}
