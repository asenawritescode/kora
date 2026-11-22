package facts

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// saleFacts is a Sale record type with lines: a Date axis, one record and one line measure,
// one record and one line dimension, and a status.
func saleFacts() *doctype.DocType {
	return &doctype.DocType{Name: "Sale", Fields: []doctype.Field{
		{Fieldname: "posting_date", Fieldtype: "Date"},
		{Fieldname: "total", Fieldtype: "Currency"},
		{Fieldname: "status", Fieldtype: "Select"},
		{Fieldname: "register", Fieldtype: "Link"},
		{Fieldname: "items", Fieldtype: "Table", Options: "Sale Item"},
	}, Facts: &doctype.FactModel{
		Time:        "posting_date",
		Lines:       "items",
		Measures:    []doctype.FactMeasure{{Field: "total"}, {Field: "items.amount", Name: "line_amount"}},
		Dimensions:  []doctype.FactDimension{{Field: "register"}, {Field: "items.item"}},
		StatusField: "status",
	}}
}

func line(name, item, amount string) map[string]any {
	return map[string]any{"name": name, "item": item, "amount": amount}
}

// summary renders a row as one deterministic line: identity, flags, date, measures, dimensions.
func summary(r Row) string {
	var m, d []string
	for k, v := range r.Measures {
		m = append(m, k+"="+v.String())
	}
	for k, v := range r.Dimensions {
		d = append(d, k+"="+v)
	}
	sort.Strings(m)
	sort.Strings(d)
	state := "live"
	if r.Deleted {
		state = "deleted"
	}
	return strings.Join([]string{
		r.RecordID, r.LineID, r.LegalEntity, r.Owner, r.BusinessDate.String(), r.Status, state,
		strings.Join(m, ","), strings.Join(d, ","),
	}, "|")
}

func TestMap(t *testing.T) {
	base := Change{RecordID: "SAL-1", Owner: "u1", Sequence: 7, OccurredAt: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}
	withLines := func(items ...any) map[string]any {
		return map[string]any{"posting_date": "2026-11-18", "total": "100.50", "status": "Paid", "register": "R1", "items": items}
	}
	tests := []struct {
		name   string
		dt     *doctype.DocType
		change Change
		clock  tenanttime.Clock
		header string
		lines  []string
	}{
		{
			name: "insert writes header and live lines",
			dt:   saleFacts(),
			change: func() Change {
				c := base
				c.Data = withLines(line("L1", "I1", "40.25"), line("L2", "I2", "60.25"))
				return c
			}(),
			header: "SAL-1|||u1|2026-11-18|Paid|live|line_amount=0,total=100.5|item=,register=R1",
			lines: []string{
				"SAL-1|L1||u1|2026-11-18|Paid|live|line_amount=40.25,total=0|item=I1,register=R1",
				"SAL-1|L2||u1|2026-11-18|Paid|live|line_amount=60.25,total=0|item=I2,register=R1",
			},
		},
		{
			name: "update tombstones a removed line and keeps the old amount",
			dt:   saleFacts(),
			change: func() Change {
				c := base
				c.Data = withLines(line("L1", "I1", "40.25"))
				c.OldData = withLines(line("L1", "I1", "40.25"), line("L2", "I2", "60.25"))
				return c
			}(),
			header: "SAL-1|||u1|2026-11-18|Paid|live|line_amount=0,total=100.5|item=,register=R1",
			lines: []string{
				"SAL-1|L1||u1|2026-11-18|Paid|live|line_amount=40.25,total=0|item=I1,register=R1",
				"SAL-1|L2||u1|2026-11-18|Paid|deleted|line_amount=60.25,total=0|item=I2,register=R1",
			},
		},
		{
			name: "delete tombstones header and every line from the old row",
			dt:   saleFacts(),
			change: func() Change {
				c := base
				c.Deleted = true
				c.OldData = withLines(line("L1", "I1", "40.25"))
				return c
			}(),
			header: "SAL-1|||u1|2026-11-18|Paid|deleted|line_amount=0,total=100.5|item=,register=R1",
			lines: []string{
				"SAL-1|L1||u1|2026-11-18|Paid|deleted|line_amount=40.25,total=0|item=I1,register=R1",
			},
		},
		{
			name: "missing measure is zero",
			dt:   saleFacts(),
			change: func() Change {
				c := base
				c.Data = map[string]any{"posting_date": "2026-11-18", "status": "Draft", "register": "R2"}
				return c
			}(),
			header: "SAL-1|||u1|2026-11-18|Draft|live|line_amount=0,total=0|item=,register=R2",
		},
		{
			name: "creation axis uses the clock zone",
			dt:   creationAxis(),
			change: func() Change {
				c := base
				c.Created = time.Date(2026, 11, 17, 22, 30, 0, 0, time.UTC)
				c.Data = map[string]any{"total": "1", "register": "R1"}
				return c
			}(),
			clock:  tenanttime.Clock{Location: mustZone(t, "Pacific/Kiritimati")},
			header: "SAL-1|||u1|2026-11-18||live|line_amount=0,total=1|item=,register=R1",
		},
		{
			name: "creation axis before the cutoff stays on the previous day",
			dt:   creationAxis(),
			change: func() Change {
				c := base
				c.Created = time.Date(2026, 11, 18, 1, 30, 0, 0, time.UTC)
				c.Data = map[string]any{"total": "1", "register": "R1"}
				return c
			}(),
			clock:  tenanttime.Clock{Location: mustZone(t, "Africa/Nairobi"), Cutoff: 5 * time.Hour},
			header: "SAL-1|||u1|2026-11-17||live|line_amount=0,total=1|item=,register=R1",
		},
		{
			name: "update adds a line as live",
			dt:   saleFacts(),
			change: func() Change {
				c := base
				c.Data = withLines(line("L1", "I1", "40.25"), line("L2", "I2", "60.25"))
				c.OldData = withLines(line("L1", "I1", "40.25"))
				return c
			}(),
			header: "SAL-1|||u1|2026-11-18|Paid|live|line_amount=0,total=100.5|item=,register=R1",
			lines: []string{
				"SAL-1|L1||u1|2026-11-18|Paid|live|line_amount=40.25,total=0|item=I1,register=R1",
				"SAL-1|L2||u1|2026-11-18|Paid|live|line_amount=60.25,total=0|item=I2,register=R1",
			},
		},
		{
			name: "datetime axis is mapped through the clock zone",
			dt:   datetimeAxis(),
			change: func() Change {
				c := base
				c.Data = map[string]any{"posting_at": "2026-11-17 22:30:00", "total": "1", "register": "R1"}
				return c
			}(),
			clock:  tenanttime.Clock{Location: mustZone(t, "Africa/Nairobi")},
			header: "SAL-1|||u1|2026-11-18||live|line_amount=0,total=1|item=,register=R1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			head, lines, err := Map(tc.dt, tc.change, tc.clock)
			if err != nil {
				t.Fatalf("map: %v", err)
			}
			if got := summary(head); got != tc.header {
				t.Fatalf("header = %s\nwant    %s", got, tc.header)
			}
			if len(lines) != len(tc.lines) {
				t.Fatalf("got %d lines, want %d", len(lines), len(tc.lines))
			}
			for i, l := range lines {
				if got := summary(l); got != tc.lines[i] {
					t.Fatalf("line %d = %s\nwant      %s", i, got, tc.lines[i])
				}
			}
		})
	}
}

// datetimeAxis is a header-only Sale whose business date comes from a Datetime field.
func datetimeAxis() *doctype.DocType {
	dt := saleFacts()
	dt.Fields = append(dt.Fields, doctype.Field{Fieldname: "posting_at", Fieldtype: "Datetime"})
	dt.Facts.Time = "posting_at"
	dt.Facts.Lines = ""
	return dt
}

// creationAxis is a header-only Sale whose business date comes from the record's creation.
func creationAxis() *doctype.DocType {
	dt := saleFacts()
	dt.Facts.Time = "creation"
	dt.Facts.Lines = ""
	return dt
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestMapMissingAxisIsZeroDate(t *testing.T) {
	tests := []struct {
		name    string
		posting any
	}{
		{"absent", nil},
		{"empty", ""},
		{"unparseable", "not a date"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			head, _, err := Map(saleFacts(), Change{RecordID: "SAL-1", Data: map[string]any{"posting_date": tc.posting}}, tenanttime.Default())
			if err != nil {
				t.Fatalf("map: %v", err)
			}
			if head.BusinessDate != (tenanttime.Date{}) {
				t.Fatalf("business date = %v, want zero", head.BusinessDate)
			}
		})
	}
}

// TestLegalEntity checks that a row takes its legal entity from the record's entity field only.
func TestLegalEntity(t *testing.T) {
	tests := []struct {
		name       string
		scopeField string
		want       string
	}{
		{"entity-owned doctype reads its entity field", "branch", "BR-1"},
		{"tenant-scoped doctype is empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dt := saleFacts()
			dt.BusinessScopeField = tc.scopeField
			dt.Fields = append(dt.Fields, doctype.Field{Fieldname: "branch", Fieldtype: "Link"})
			change := Change{RecordID: "SAL-1", Owner: "u1", Sequence: 7, OccurredAt: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC),
				Data: map[string]any{"posting_date": "2026-11-18", "branch": "BR-1"}}
			head, _, err := Map(dt, change, tenanttime.Clock{})
			if err != nil {
				t.Fatalf("map: %v", err)
			}
			if head.LegalEntity != tc.want {
				t.Fatalf("LegalEntity = %q, want %q", head.LegalEntity, tc.want)
			}
		})
	}
}

// TestExcludedStatus checks that a record in an excluded status maps to tombstones, and that the same
// record in an included status maps to live rows again.
func TestExcludedStatus(t *testing.T) {
	held := func(status string) Change {
		return Change{RecordID: "SAL-1", Owner: "u1", Sequence: 7, OccurredAt: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC),
			Data: map[string]any{"posting_date": "2026-11-18", "status": status, "total": "100.50", "register": "R1",
				"items": []any{line("L1", "I1", "40.25")}}}
	}
	dt := saleFacts()
	dt.Facts.ExcludeStatus = []string{"Held"}
	tests := []struct {
		name   string
		change Change
		header string
		lines  []string
	}{
		{
			name:   "held record is tombstoned",
			change: held("Held"),
			header: "SAL-1|||u1|2026-11-18|Held|deleted|line_amount=0,total=100.5|item=,register=R1",
			lines:  []string{"SAL-1|L1||u1|2026-11-18|Held|deleted|line_amount=40.25,total=0|item=I1,register=R1"},
		},
		{
			name:   "record moving to Paid writes live rows",
			change: func() Change { c := held("Paid"); c.OldData = held("Held").Data; return c }(),
			header: "SAL-1|||u1|2026-11-18|Paid|live|line_amount=0,total=100.5|item=,register=R1",
			lines:  []string{"SAL-1|L1||u1|2026-11-18|Paid|live|line_amount=40.25,total=0|item=I1,register=R1"},
		},
		{
			name:   "paid record moving to Held is tombstoned",
			change: func() Change { c := held("Held"); c.OldData = held("Paid").Data; return c }(),
			header: "SAL-1|||u1|2026-11-18|Held|deleted|line_amount=0,total=100.5|item=,register=R1",
			lines:  []string{"SAL-1|L1||u1|2026-11-18|Held|deleted|line_amount=40.25,total=0|item=I1,register=R1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			head, lines, err := Map(dt, tc.change, tenanttime.Clock{})
			if err != nil {
				t.Fatalf("map: %v", err)
			}
			if got := summary(head); got != tc.header {
				t.Errorf("header = %s\nwant    %s", got, tc.header)
			}
			if len(lines) != len(tc.lines) {
				t.Fatalf("lines = %d, want %d", len(lines), len(tc.lines))
			}
			for i, l := range lines {
				if got := summary(l); got != tc.lines[i] {
					t.Errorf("line %d = %s\nwant       %s", i, got, tc.lines[i])
				}
			}
		})
	}
}

// A child line without a name has no id, so Map refuses it on either side of a change: a line row is never
// written under an empty id, which would collide with every other line of the record.
func TestMapRejectsLineWithoutName(t *testing.T) {
	nameless := map[string]any{"item": "I1", "amount": "40.25"}
	tests := []struct {
		name   string
		change Change
	}{
		{"insert", Change{RecordID: "SAL-1", Data: map[string]any{"posting_date": "2026-11-18", "items": []any{nameless}}}},
		{"update removes a nameless old line", Change{RecordID: "SAL-1",
			Data:    map[string]any{"posting_date": "2026-11-18", "items": []any{line("L1", "I1", "40.25")}},
			OldData: map[string]any{"posting_date": "2026-11-18", "items": []any{nameless}}}},
		{"delete", Change{RecordID: "SAL-1", Deleted: true, OldData: map[string]any{"posting_date": "2026-11-18", "items": []any{nameless}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Map(saleFacts(), tc.change, tenanttime.Default())
			if !errors.Is(err, errLineWithoutID) {
				t.Fatalf("error = %v, want errLineWithoutID", err)
			}
		})
	}
}
