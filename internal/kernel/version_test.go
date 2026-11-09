package kernel

import (
	"testing"
	"time"
)

func TestCanonicalVersionNormalizesDatabaseTimestampRepresentations(t *testing.T) {
	const want = "2026-10-02T03:04:05.123Z"
	values := []any{
		"2026-10-02 03:04:05.123",
		[]byte("2026-10-02 03:04:05.123"),
		time.Date(2026, 10, 2, 3, 4, 5, 123_000_000, time.UTC),
	}
	for _, value := range values {
		if got := CanonicalVersion(value); got != want {
			t.Errorf("CanonicalVersion(%T) = %q, want %q", value, got, want)
		}
	}
}

func TestParseVersionTimestampRejectsUnknownDatabaseValue(t *testing.T) {
	if _, err := parseVersionTimestamp("not-a-timestamp"); err == nil {
		t.Fatal("expected invalid timestamp to fail")
	}
}
