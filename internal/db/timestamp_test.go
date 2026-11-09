package db

import (
	"testing"
	"time"
)

func TestTimestampScan(t *testing.T) {
	want := time.Date(2026, 10, 15, 12, 34, 56, 123000000, time.UTC)
	tests := []struct {
		name  string
		value any
	}{
		{name: "native", value: want},
		{name: "sqlite string", value: "2026-10-15 12:34:56.123"},
		{name: "sqlite bytes", value: []byte("2026-10-15 12:34:56.123")},
		{name: "rfc3339", value: "2026-10-15T12:34:56.123Z"},
		{name: "offset", value: "2026-10-15 15:34:56.123+03:00"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got Timestamp
			if err := got.Scan(test.value); err != nil {
				t.Fatalf("Scan(%T) error = %v", test.value, err)
			}
			if !got.Equal(want) {
				t.Fatalf("Scan(%T) = %s, want %s", test.value, got.UTC(), want)
			}
		})
	}
}

func TestTimestampRejectsInvalidValues(t *testing.T) {
	for _, test := range []struct {
		name  string
		value any
	}{
		{name: "null", value: nil},
		{name: "unsupported", value: int64(1)},
		{name: "invalid", value: "not-a-time"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got Timestamp
			if err := got.Scan(test.value); err == nil {
				t.Fatalf("Scan(%#v) succeeded, want error", test.value)
			}
		})
	}
}
