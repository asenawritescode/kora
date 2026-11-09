package sqlstore

import (
	"testing"
	"time"
)

func TestParseDBTimeAcceptsLibSQLHTTPOffsetTimestamp(t *testing.T) {
	got, err := parseDBTime("2026-10-15 10:34:03.281+00:00")
	if err != nil {
		t.Fatal("parse LibSQL HTTP timestamp:", err)
	}
	want := time.Date(2026, 10, 15, 10, 34, 3, 281000000, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("parsed timestamp = %s, want %s", got, want)
	}
}
