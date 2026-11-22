package metrics

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// drillPageSize is the number of records one drill page lists. A variable so a test can page over few records.
var drillPageSize = 50

// ErrBadCursor marks a drill cursor that no drill page issued.
var ErrBadCursor = errors.New("metrics: drill cursor is not valid")

// DrillRefusal is a drill the catalog cannot answer, or a group the caller may not ask for. Its message is the cause's.
type DrillRefusal struct{ cause error }

func (r DrillRefusal) Error() string { return r.cause.Error() }

func (r DrillRefusal) Unwrap() error { return r.cause }

// DrillPage is one page of the records behind a drill number. Total is the number the drill names: the
// bucket's value in the query, which the records of all pages add up to. Next is the cursor of the
// following page, empty on the last one.
type DrillPage struct {
	Drill
	Total   decimal.Decimal
	Records []DrillRecord
	Next    string
}

// DrillRecord is one record behind a drill number: its name and title, its time, and its share of the number.
type DrillRecord struct {
	Name       string
	Title      string
	OccurredAt time.Time
	Value      decimal.Decimal
}

// DrillRecords lists the records behind req, newest first, one page per call. Cursor is empty for the
// first page and the Next of the page before for the rest. The records are read from the fact rows with
// the conditions and group values of the bucket, so Total is the number the query reports for them.
func (e Engine) DrillRecords(ctx context.Context, req DrillRequest, scope Scope, clock tenanttime.Clock, cursor string) (DrillPage, error) {
	d, err := e.Catalog.Drill(req, clock)
	if err != nil {
		return DrillPage{}, DrillRefusal{err}
	}
	after, err := decodeCursor(cursor)
	if err != nil {
		return DrillPage{}, err
	}
	p, err := planQuery(e.Catalog, Query{Metrics: []string{req.Metric}, Dimensions: slices.Sorted(maps.Keys(req.Group)),
		From: req.From, To: req.To, Grain: GrainRange, Scope: scope})
	if err != nil {
		return DrillPage{}, DrillRefusal{err}
	}
	p.summary = false
	ctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()
	page := DrillPage{Drill: d}
	if page.Total, err = e.drillTotal(ctx, p, req.Group); err != nil {
		return DrillPage{}, err
	}
	rows, err := e.drillRows(ctx, p, req.Group, after)
	if err != nil {
		return DrillPage{}, err
	}
	if len(rows) > drillPageSize {
		rows = rows[:drillPageSize]
		last := rows[drillPageSize-1]
		page.Next = encodeCursor(last.at, last.name)
	}
	titles, err := e.titles(ctx, e.Catalog.Models[d.Doctype], recordNames(rows))
	if err != nil {
		return DrillPage{}, err
	}
	page.Records = make([]DrillRecord, len(rows))
	for i, r := range rows {
		page.Records[i] = DrillRecord{Name: r.name, Title: titles[r.name], OccurredAt: r.at, Value: r.value}
	}
	return page, nil
}

// drillRow is one record of a drill before its title is read.
type drillRow struct {
	name  string
	at    time.Time
	value decimal.Decimal
}

// recordNames lists the record names of a page, in page order.
func recordNames(rows []drillRow) []string {
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.name
	}
	return names
}

// drillTotal sums the records of a drill: the number the drill names.
func (e Engine) drillTotal(ctx context.Context, p plan, group map[string]string) (decimal.Decimal, error) {
	q := e.Dialect.QuoteIdent
	from, args := p.recordsFrom(e.Dialect, group)
	stmt := "SELECT COALESCE(SUM(r." + q("value") + "), 0) FROM (" + from + ") AS r"
	var total decimal.Decimal
	if err := e.DB.QueryRowContext(ctx, db.Rebind(e.Dialect, stmt), args...).Scan(&total); err != nil {
		return decimal.Decimal{}, fmt.Errorf("metrics: total drill records: %w", err)
	}
	return total, nil
}

// drillRows reads one page of records after the cursor, newest first, with one extra row to know
// whether another page follows.
func (e Engine) drillRows(ctx context.Context, p plan, group map[string]string, after *drillCursor) ([]drillRow, error) {
	q := e.Dialect.QuoteIdent
	from, args := p.recordsFrom(e.Dialect, group)
	at, id := "r."+q("occurred_at"), "r."+q("record_id")
	var where string
	if after != nil {
		where = " WHERE (" + at + " < ? OR (" + at + " = ? AND " + id + " < ?))"
		args = append(args, after.at, after.at, after.name)
	}
	stmt := "SELECT " + id + ", " + at + ", r." + q("value") + " FROM (" + from + ") AS r" + where +
		" ORDER BY " + at + " DESC, " + id + " DESC LIMIT ?"
	args = append(args, drillPageSize+1)
	rows, err := e.DB.QueryContext(ctx, db.Rebind(e.Dialect, stmt), args...)
	if err != nil {
		return nil, fmt.Errorf("metrics: list drill records: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []drillRow
	for rows.Next() {
		var r drillRow
		if err := rows.Scan(&r.name, &r.at, &r.value); err != nil {
			return nil, fmt.Errorf("metrics: scan drill records: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("metrics: list drill records: %w", err)
	}
	return out, nil
}

// recordsFrom is the derived table of a drill: one row per record the bucket counts, with the record's
// newest occurred_at and its value. It applies the conditions the bucket's series applies to its rows,
// and the group values, so its values sum to the bucket's number.
func (p plan) recordsFrom(d db.Dialect, group map[string]string) (string, []any) {
	const f = "f."
	q := d.QuoteIdent
	rp := p
	rp.records = true
	var args []any
	s := p.series[0]
	// Build one union only: each fact part appends its arguments, so a discarded part would leave extra args.
	var union string
	if s.line && !p.hasLineDim() {
		// A line sum counts line rows only: a header row has no line value, and a record without a matching
		// line is not in the number.
		union = rp.factsPart(d, p.model.Facts.LineTableName(p.model), true, &args)
	} else {
		union = rp.factsUnion(d, &args)
	}
	value := "1"
	conds := s.conds
	if s.agg == aggSum {
		value = "SUM(" + f + q(s.column) + ")"
	}
	if s.agg == aggCount && !p.hasLineDim() {
		conds = append([]predicate{{field: "line_id", op: "=", value: ""}}, conds...)
	}
	var where []string
	if len(conds) > 0 {
		where = append(where, p.condition(d, f, conds, &args))
	}
	for _, dim := range p.dims {
		where = append(where, f+q(dim.alias)+" = ?")
		args = append(args, group[dim.name])
	}
	stmt := "SELECT " + f + q("record_id") + " AS " + q("record_id") + ", MAX(" + f + q("occurred_at") + ") AS " +
		q("occurred_at") + ", " + value + " AS " + q("value") + " FROM (" + union + ") AS f"
	if len(where) > 0 {
		stmt += " WHERE " + strings.Join(where, " AND ")
	}
	return stmt + " GROUP BY " + f + q("record_id"), args
}

// drillCursor is the position after the last record of a page: the sort key the next page starts below.
type drillCursor struct {
	at   time.Time
	name string
}

// encodeCursor writes the position of a record as an opaque cursor.
func encodeCursor(at time.Time, name string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + name))
}

// decodeCursor reads a cursor. The empty cursor is the first page and has no position.
func decodeCursor(s string) (*drillCursor, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, ErrBadCursor
	}
	stamp, name, ok := strings.Cut(string(raw), "|")
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if !ok || err != nil || name == "" {
		return nil, ErrBadCursor
	}
	return &drillCursor{at: at, name: name}, nil
}
