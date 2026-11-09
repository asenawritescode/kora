package api

import (
	"testing"
	"time"
)

func TestParseRealtimeTimestampAcceptsSupportedDatabaseValues(t *testing.T) {
	want := time.Date(2026, time.October, 14, 15, 18, 50, 123000000, time.UTC)
	for _, test := range []struct {
		name  string
		value any
	}{
		{name: "time.Time", value: want},
		{name: "RFC3339 text", value: "2026-10-14T15:18:50.123Z"},
		{name: "SQLite datetime text", value: "2026-10-14 15:18:50.123"},
		{name: "MySQL datetime text", value: []byte("2026-10-14 15:18:50.123000")},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseRealtimeTimestamp(test.value)
			if err != nil {
				t.Fatal("parse timestamp:", err)
			}
			if !got.Equal(want) {
				t.Fatalf("timestamp = %s, want %s", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
			}
		})
	}
}

func TestParseRealtimeTimestampRejectsInvalidValues(t *testing.T) {
	for _, value := range []any{nil, 42, "not-a-timestamp"} {
		if _, err := parseRealtimeTimestamp(value); err == nil {
			t.Errorf("parseRealtimeTimestamp(%#v) unexpectedly succeeded", value)
		}
	}
}
