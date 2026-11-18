package facts

import (
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/numeric"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// Change is one decoded record event; the caller fills Owner and Created (not in the payload).
// A delete has Data nil and Deleted set.
type Change struct {
	RecordID    string
	LegalEntity string
	Owner       string
	Created     time.Time
	Sequence    int64
	OccurredAt  time.Time
	Deleted     bool
	Data        map[string]any
	OldData     map[string]any
}

// Row is one fact row with a value for every column. Measures and Dimensions are keyed
// by column name.
type Row struct {
	RecordID       string
	LineID         string
	LegalEntity    string
	Owner          string
	BusinessDate   tenanttime.Date
	OccurredAt     time.Time
	Status         string
	SourceSequence int64
	Deleted        bool
	Measures       map[string]decimal.Decimal
	Dimensions     map[string]string
}

// Map turns a change into its header row and, if Lines is set, one row per child line.
// Dropped lines and every line of a delete are tombstones. A bad axis value maps to the zero Date.
func Map(dt *doctype.DocType, ev Change, clock tenanttime.Clock) (Row, []Row) {
	f := dt.Facts
	src := ev.Data
	if ev.Deleted || src == nil {
		src = ev.OldData
	}
	head := newRow(dt, ev, src, clock)
	head.Measures = measureValues(f.Measures, src, nil)
	head.Dimensions = dimensionValues(f.Dimensions, src, nil)
	if f.Lines == "" {
		return head, nil
	}
	var rows []Row
	seen := map[string]bool{}
	for _, child := range childRows(src, f.Lines) {
		seen[text(child["name"])] = true
		rows = append(rows, lineRow(head, f, src, child, ev.Deleted))
	}
	if !ev.Deleted {
		for _, child := range childRows(ev.OldData, f.Lines) {
			if !seen[text(child["name"])] {
				rows = append(rows, lineRow(head, f, src, child, true))
			}
		}
	}
	return head, rows
}

// lineRow fills a copy of the header with one child line; header measures stay on the header.
func lineRow(head Row, f *doctype.FactModel, rec, child map[string]any, deleted bool) Row {
	head.LineID = text(child["name"])
	head.Deleted = deleted
	head.Measures = measureValues(f.Measures, nil, child)
	head.Dimensions = dimensionValues(f.Dimensions, rec, child)
	return head
}

// newRow fills the columns every row shares.
func newRow(dt *doctype.DocType, ev Change, src map[string]any, clock tenanttime.Clock) Row {
	status := ""
	if dt.Facts.StatusField != "" {
		status = text(src[dt.Facts.StatusField])
	}
	return Row{
		RecordID:       ev.RecordID,
		LegalEntity:    ev.LegalEntity,
		Owner:          ev.Owner,
		BusinessDate:   businessDate(dt, ev, src, clock),
		OccurredAt:     ev.OccurredAt.UTC(),
		Status:         status,
		SourceSequence: ev.Sequence,
		Deleted:        ev.Deleted,
	}
}

// businessDate reads the time axis: creation, a Date day as stored, or a Datetime through the clock.
// A missing or unparseable axis value is the zero Date, which the consumer rejects.
func businessDate(dt *doctype.DocType, ev Change, src map[string]any, clock tenanttime.Clock) tenanttime.Date {
	f := dt.Facts
	t := ev.Created
	if f.Time != "creation" {
		t = instant(src[f.Time])
		if fld := dt.GetField(f.Time); fld != nil && fld.Fieldtype == "Date" && !t.IsZero() {
			return tenanttime.Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}
		}
	}
	if t.IsZero() {
		return tenanttime.Date{}
	}
	return clock.BusinessDate(t)
}

// instant parses a stored Date or Datetime; naive values are UTC.
func instant(v any) time.Time {
	if t, ok := v.(time.Time); ok {
		return t
	}
	if b, ok := v.([]byte); ok {
		v = string(b)
	}
	s, _ := v.(string)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// measureValues reads each measure from rec or line; an absent value is zero.
func measureValues(ms []doctype.FactMeasure, rec, line map[string]any) map[string]decimal.Decimal {
	out := make(map[string]decimal.Decimal, len(ms))
	for _, m := range ms {
		d, _ := numeric.Parse(valueAt(m.Field, rec, line))
		out[m.ColumnName()] = d
	}
	return out
}

// dimensionValues reads each dimension the same way measureValues reads measures.
func dimensionValues(ds []doctype.FactDimension, rec, line map[string]any) map[string]string {
	out := make(map[string]string, len(ds))
	for _, d := range ds {
		out[d.ColumnName()] = text(valueAt(d.Field, rec, line))
	}
	return out
}

// valueAt resolves a record or "lines.name" path against the record or the line.
func valueAt(path string, rec, line map[string]any) any {
	if _, leaf, dotted := strings.Cut(path, "."); dotted {
		return line[leaf]
	}
	return rec[path]
}

// text renders a dimension value; an absent value is the empty string.
func text(v any) string {
	if v == nil {
		return ""
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// childRows returns the child rows under field, from decoded JSON or Go maps.
func childRows(m map[string]any, field string) []map[string]any {
	switch v := m[field].(type) {
	case []map[string]any:
		return v
	case []any:
		var rows []map[string]any
		for _, item := range v {
			if row, ok := item.(map[string]any); ok {
				rows = append(rows, row)
			}
		}
		return rows
	}
	return nil
}
