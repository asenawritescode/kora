package metrics

import (
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/tenanttime"
)

// nairobiClock is Africa/Nairobi with the 04:00 business-day cutoff the shops use.
func nairobiClock(t *testing.T) tenanttime.Clock {
	t.Helper()
	loc, err := time.LoadLocation("Africa/Nairobi")
	if err != nil {
		t.Fatal(err)
	}
	return tenanttime.Clock{Location: loc, Cutoff: 4 * time.Hour}
}

// nairobi is a wall-clock instant in Africa/Nairobi.
func nairobi(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.ParseInLocation("2006-01-02 15:04", s, nairobiClock(t).Location)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func TestResolvePeriodPresetsOnTheBusinessDay(t *testing.T) {
	tests := []struct {
		name   string
		now    string
		preset Preset
		from   tenanttime.Date
		to     tenanttime.Date
	}{
		{"today after the cutoff", "2026-11-21 05:00", PresetToday, day(2026, 11, 21), day(2026, 11, 21)},
		{"today before the cutoff is still yesterday", "2026-11-21 00:30", PresetToday, day(2026, 11, 20), day(2026, 11, 20)},
		{"yesterday before the cutoff is the day before that", "2026-11-21 00:30", PresetYesterday, day(2026, 11, 19), day(2026, 11, 19)},
		{"this week on Monday after the cutoff", "2026-11-16 10:00", PresetThisWeek, day(2026, 11, 16), day(2026, 11, 16)},
		{"this week on Monday before the cutoff is Sunday's week", "2026-11-16 02:00", PresetThisWeek, day(2026, 11, 9), day(2026, 11, 15)},
		{"last week on Monday after the cutoff", "2026-11-16 10:00", PresetLastWeek, day(2026, 11, 9), day(2026, 11, 15)},
		{"last week on Sunday night before the cutoff", "2026-11-16 02:00", PresetLastWeek, day(2026, 11, 2), day(2026, 11, 8)},
		{"this month on the first", "2026-03-01 06:00", PresetThisMonth, day(2026, 3, 1), day(2026, 3, 1)},
		{"last month from March is February", "2026-03-01 06:00", PresetLastMonth, day(2026, 2, 1), day(2026, 2, 28)},
		{"last month from March of a leap year", "2028-03-01 06:00", PresetLastMonth, day(2028, 2, 1), day(2028, 2, 29)},
		{"last month from the 31st does not overflow into March", "2026-03-31 12:00", PresetLastMonth, day(2026, 2, 1), day(2026, 2, 28)},
		{"last month from January is December of the year before", "2026-01-31 12:00", PresetLastMonth, day(2025, 12, 1), day(2025, 12, 31)},
		{"last 7 days ends today", "2026-11-21 12:00", PresetLast7Days, day(2026, 11, 15), day(2026, 11, 21)},
		{"last 30 days crosses a month", "2026-11-21 12:00", PresetLast30Days, day(2026, 10, 23), day(2026, 11, 21)},
		{"last 3 months starts on the first of the month two back", "2026-11-21 12:00", PresetLast3Months, day(2026, 9, 1), day(2026, 11, 21)},
		{"last 3 months from January crosses the year", "2026-01-15 12:00", PresetLast3Months, day(2025, 11, 1), day(2026, 1, 15)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolvePeriod(nairobiClock(t), nairobi(t, tc.now), tc.preset, Span{})
			if err != nil {
				t.Fatal(err)
			}
			if want := (Span{tc.from, tc.to}); got != want {
				t.Fatalf("span = %s..%s, want %s..%s", got.From, got.To, want.From, want.To)
			}
		})
	}
}

func TestResolvePeriodCustomAndUnknown(t *testing.T) {
	clock := nairobiClock(t)
	now := nairobi(t, "2026-11-21 12:00")
	custom := Span{day(2026, 11, 1), day(2026, 11, 5)}
	if got, err := ResolvePeriod(clock, now, PresetCustom, custom); err != nil || got != custom {
		t.Fatalf("custom = %v, %v; want the given span", got, err)
	}
	if _, err := ResolvePeriod(clock, now, Preset("fortnight"), Span{}); err == nil {
		t.Fatal("unknown preset resolved")
	}
}

func TestParsePreset(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"today", false}, {"last_3_months", false}, {"custom", false},
		{"", true}, {"last_year", true}, {"Today", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParsePreset(tc.name); (err != nil) != tc.wantErr {
				t.Fatalf("ParsePreset(%q) err = %v, want error %v", tc.name, err, tc.wantErr)
			}
		})
	}
}

func TestWeekBeforeShiftsSevenDays(t *testing.T) {
	tests := []struct {
		name string
		s    Span
		want Span
	}{
		{"single day compares with the same weekday last week", Span{day(2026, 11, 21), day(2026, 11, 21)}, Span{day(2026, 11, 14), day(2026, 11, 14)}},
		{"range shifts both ends", Span{day(2026, 11, 18), day(2026, 11, 20)}, Span{day(2026, 11, 11), day(2026, 11, 13)}},
		{"across a month end", Span{day(2026, 3, 3), day(2026, 3, 4)}, Span{day(2026, 2, 24), day(2026, 2, 25)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.weekBefore(); got != tc.want {
				t.Fatalf("weekBefore = %s..%s, want %s..%s", got.From, got.To, tc.want.From, tc.want.To)
			}
		})
	}
}
