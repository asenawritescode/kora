package tenanttime

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, layout, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(layout, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestClockBusinessDate(t *testing.T) {
	nairobi := Clock{Location: defaultLocation}
	threeAM := Clock{Location: defaultLocation, Cutoff: 3 * time.Hour}
	for _, tt := range []struct {
		name  string
		clock Clock
		at    string
		want  string
	}{
		{"late evening is same day", nairobi, "2026-11-18T19:30:00Z", "2026-11-18"}, // 22:30 EAT
		{"before cutoff is previous day", threeAM, "2026-11-18T23:30:00Z", "2026-11-18"},
		{"02:30 local with 03:00 cutoff", threeAM, "2026-11-18T23:30:00Z", "2026-11-18"}, // 02:30 EAT on the 19th
		{"at cutoff starts the day", threeAM, "2026-11-19T00:00:00Z", "2026-11-19"},      // 03:00 EAT on the 19th
		{"zero value uses Nairobi", Clock{}, "2026-11-18T21:30:00Z", "2026-11-19"},       // 00:30 EAT on the 19th
		{"London BST day", Clock{Location: mustLoad(t, "Europe/London")}, "2026-07-01T23:30:00Z", "2026-07-02"},
		{"London GMT day", Clock{Location: mustLoad(t, "Europe/London")}, "2026-01-15T23:30:00Z", "2026-01-15"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.clock.BusinessDate(mustParse(t, time.RFC3339, tt.at))
			if got.String() != tt.want {
				t.Fatalf("BusinessDate(%s) = %s, want %s", tt.at, got, tt.want)
			}
		})
	}
}

func TestDateString(t *testing.T) {
	if got := (Date{2026, time.February, 3}).String(); got != "2026-02-03" {
		t.Fatalf("String() = %s", got)
	}
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
