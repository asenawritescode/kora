package metrics

import (
	"fmt"
	"slices"
	"time"

	"github.com/asenawritescode/kora/internal/tenanttime"
)

// Preset is a named period the server resolves from the tenant's business clock, so a saved question
// reruns relative to today. Custom takes the from and to dates of the request instead.
type Preset string

// Named periods. Weeks start on Monday. This week ends today; last week is the previous Monday to Sunday.
const (
	PresetToday       Preset = "today"
	PresetYesterday   Preset = "yesterday"
	PresetThisWeek    Preset = "this_week"
	PresetLastWeek    Preset = "last_week"
	PresetThisMonth   Preset = "this_month"
	PresetLastMonth   Preset = "last_month"
	PresetLast7Days   Preset = "last_7_days"
	PresetLast30Days  Preset = "last_30_days"
	PresetLast3Months Preset = "last_3_months"
	PresetCustom      Preset = "custom"
)

// presets lists every preset, custom included, in display order.
var presets = []Preset{PresetToday, PresetYesterday, PresetThisWeek, PresetLastWeek, PresetThisMonth,
	PresetLastMonth, PresetLast7Days, PresetLast30Days, PresetLast3Months, PresetCustom}

// Span is an inclusive range of business dates.
type Span struct {
	From, To tenanttime.Date
}

// ParsePreset reads a preset name, or reports that it names none.
func ParsePreset(name string) (Preset, error) {
	p := Preset(name)
	if !slices.Contains(presets, p) {
		return "", fmt.Errorf("period %q is not a preset", name)
	}
	return p, nil
}

// ResolvePeriod is the span a preset names on the business day that contains now in clock. Custom names
// no span of its own: it returns custom, which the caller read from the request.
func ResolvePeriod(clock tenanttime.Clock, now time.Time, preset Preset, custom Span) (Span, error) {
	if preset == PresetCustom {
		return custom, nil
	}
	span, ok := preset.span(clock.BusinessDate(now))
	if !ok {
		return Span{}, fmt.Errorf("period %q is not a preset", preset)
	}
	return span, nil
}

// presetNames lists the presets a dashboard may default to: every preset but custom.
func presetNames() []string {
	var out []string
	for _, p := range presets {
		if p != PresetCustom {
			out = append(out, string(p))
		}
	}
	return out
}

// span is the span a non-custom preset names, counting from the business day today. It reports false
// for custom and for a name that is no preset.
func (p Preset) span(today tenanttime.Date) (Span, bool) {
	t := toTime(today)
	day := func(n int) tenanttime.Date { return toDate(t.AddDate(0, 0, n)) }
	// The first day of the month n months from today's. Date-only time.Date normalises a month overflow,
	// so day 31 never skips into the next month the way AddDate would.
	month := func(n int) tenanttime.Date {
		return toDate(time.Date(t.Year(), t.Month()+time.Month(n), 1, 0, 0, 0, 0, time.UTC))
	}
	monday := periodStart(t, GrainWeek)
	switch p {
	case PresetToday:
		return Span{today, today}, true
	case PresetYesterday:
		return Span{day(-1), day(-1)}, true
	case PresetThisWeek:
		return Span{toDate(monday), today}, true
	case PresetLastWeek:
		return Span{toDate(monday.AddDate(0, 0, -7)), toDate(monday.AddDate(0, 0, -1))}, true
	case PresetThisMonth:
		return Span{month(0), today}, true
	case PresetLastMonth:
		return Span{month(-1), toDate(toTime(month(0)).AddDate(0, 0, -1))}, true
	case PresetLast7Days:
		return Span{day(-6), today}, true
	case PresetLast30Days:
		return Span{day(-29), today}, true
	case PresetLast3Months:
		return Span{month(-2), today}, true
	case PresetCustom:
		return Span{}, false
	}
	return Span{}, false
}

// weekBefore is the same dates one week earlier. A single day compares with the same weekday last week.
func (s Span) weekBefore() Span {
	return Span{toDate(toTime(s.From).AddDate(0, 0, -7)), toDate(toTime(s.To).AddDate(0, 0, -7))}
}
