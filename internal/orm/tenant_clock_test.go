package orm

import (
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

func TestConvertDefaultToday(t *testing.T) {
	for _, tt := range []struct {
		name      string
		def       string
		fieldtype string
		today     string
		want      any
	}{
		{"Date today uses the business day", "today", "Date", "2026-11-18", "2026-11-18"},
		{"Date today is case-insensitive", "TODAY", "Date", "2026-11-18", "2026-11-18"},
		{"Date literal is kept", "2026-01-01", "Date", "2026-11-18", "2026-01-01"},
		{"Datetime ignores the business day", "2026-11-18", "Datetime", "2026-11-17", "2026-11-18"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := convertDefault(tt.def, tt.fieldtype, tt.today); got != tt.want {
				t.Fatalf("convertDefault(%q, %q) = %v, want %v", tt.def, tt.fieldtype, got, tt.want)
			}
		})
	}
}

func TestTodayDefaultFollowsTenantClock(t *testing.T) {
	// 2026-11-18 02:30 Nairobi with a 03:00 cutoff is still the 17th.
	clock := tenanttime.Clock{Location: mustNairobi(t), Cutoff: 3 * time.Hour}
	for _, tt := range []struct {
		name      string
		fieldtype string
		def       string
		want      string
	}{
		{"today on a Date field", "Date", "today", clockDay(t, clock)},
		{"other fields get no day", "Datetime", "today", ""},
		{"Date with a literal default gets no day", "Date", "2026-01-01", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tx := &TxManager{clock: &clock}
			if got := tx.todayDefault(tt.fieldtype, tt.def); got != tt.want {
				t.Fatalf("todayDefault(%q, %q) = %q, want %q", tt.fieldtype, tt.def, got, tt.want)
			}
		})
	}
}

func TestBusinessClockFallsBackToDefault(t *testing.T) {
	tx := &TxManager{SiteName: "s1"}
	if got := tx.businessClock(); got.Location.String() != tenanttime.Default().Location.String() || got.Cutoff != 0 {
		t.Fatalf("businessClock() without a database = %+v, want default", got)
	}
}

func TestApplyChildTodayDefaultsFillsOnlyMissingRows(t *testing.T) {
	childDT := &doctype.DocType{
		Name:         "Visit Line",
		IsChildTable: true,
		Fields: []doctype.Field{
			{Fieldname: "visited_on", Fieldtype: "Date", Default: "today"},
			{Fieldname: "note", Fieldtype: "Data"},
		},
	}
	clock := tenanttime.Clock{Location: mustNairobi(t)}
	tx := &TxManager{clock: &clock}
	filled := doctype.NewDocument("Visit Line")
	kept := doctype.NewDocument("Visit Line")
	kept.Set("visited_on", "2020-02-02")

	tx.applyChildTodayDefaults(childDT, []*doctype.Document{filled, kept})

	if got, want := filled.Get("visited_on"), clockDay(t, clock); got != want {
		t.Fatalf("missing child date = %v, want %s", got, want)
	}
	if got := kept.Get("visited_on"); got != "2020-02-02" {
		t.Fatalf("existing child date overwritten: %v", got)
	}
}

func mustNairobi(t *testing.T) *time.Location {
	t.Helper()
	return tenanttime.Default().Location
}

func clockDay(t *testing.T, clock tenanttime.Clock) string {
	t.Helper()
	return clock.BusinessDate(time.Now()).String()
}
