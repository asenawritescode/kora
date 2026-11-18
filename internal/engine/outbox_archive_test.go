package engine

import (
	"context"
	"testing"
	"time"
)

func TestArchiveFirstDelayStaysWithinFirstHour(t *testing.T) {
	tests := []struct {
		name   string
		random time.Duration
		want   time.Duration
	}{
		{name: "zero draw starts at once", random: 0, want: 0},
		{name: "mid-hour draw is used as is", random: 30 * time.Minute, want: 30 * time.Minute},
		{name: "negative draw clamps to zero", random: -time.Second, want: 0},
		{name: "draw at the bound clamps below an hour", random: time.Hour, want: time.Hour - time.Nanosecond},
		{name: "draw past the bound clamps below an hour", random: 2 * time.Hour, want: time.Hour - time.Nanosecond},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var asked time.Duration
			got := archiveFirstDelay(func(n time.Duration) time.Duration {
				asked = n
				return tc.random
			})
			if asked != time.Hour {
				t.Fatalf("random asked for %v, want %v", asked, time.Hour)
			}
			if got != tc.want {
				t.Fatalf("archiveFirstDelay = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRunOutboxArchiverStopsOnContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		// A nil archiver is never reached: the context is already done.
		RunOutboxArchiver(ctx, "acme", nil, nil, func(time.Duration) time.Duration { return time.Hour })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunOutboxArchiver did not return after context cancellation")
	}
}
