package cli

import (
	"testing"
	"time"
)

func TestOutboxArchiveHorizonFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "unset uses the 400-day default", raw: "", want: 400 * 24 * time.Hour},
		{name: "blank uses the default", raw: "   ", want: 400 * 24 * time.Hour},
		{name: "minimum 90 is accepted", raw: "90", want: 90 * 24 * time.Hour},
		{name: "explicit value", raw: "365", want: 365 * 24 * time.Hour},
		{name: "below minimum is rejected", raw: "89", wantErr: true},
		{name: "zero is rejected", raw: "0", wantErr: true},
		{name: "negative is rejected", raw: "-400", wantErr: true},
		{name: "junk is rejected", raw: "four hundred", wantErr: true},
		{name: "fractional days are rejected", raw: "400.5", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KORA_OUTBOX_ARCHIVE_DAYS", tc.raw)
			got, err := outboxArchiveHorizonFromEnv()
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %t", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("horizon = %v, want %v", got, tc.want)
			}
		})
	}
}
