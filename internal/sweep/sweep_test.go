package sweep

import (
	"context"
	"testing"
	"time"
)

// neverFires is a sweep that is never due, so only a wake can run a pass.
func neverFires(time.Duration) (<-chan time.Time, func()) { return make(chan time.Time), func() {} }

func TestWakeRunsPassWithinLatencyBound(t *testing.T) {
	wake := NewWake()
	timer := New(time.Hour, wake.Subscribe())
	timer.after = neverFires
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := make(chan time.Time, 4)
	go Loop(ctx, timer, func(context.Context) bool {
		ran <- time.Now()
		return false
	})
	// The first pass waits for the random initial offset; take it with a wake.
	wake.Signal()
	<-ran
	start := time.Now()
	wake.Signal()
	select {
	case at := <-ran:
		if lat := at.Sub(start); lat > 100*time.Millisecond {
			t.Fatalf("wake-to-pass latency = %v, want < 100ms", lat)
		}
	case <-time.After(time.Second):
		t.Fatal("commit wake did not run a pass")
	}
}

func TestNilWakeIsNoOp(t *testing.T) {
	var w *Wake
	w.Signal()
	if ch := w.Subscribe(); ch != nil {
		t.Fatalf("nil Wake Subscribe = %v, want nil channel", ch)
	}
}

func TestSignalCoalescesPerSubscriber(t *testing.T) {
	wake := NewWake()
	ch := wake.Subscribe()
	for range 50 {
		wake.Signal()
	}
	<-ch
	select {
	case <-ch:
		t.Fatal("burst of signals left more than one pending wake")
	default:
	}
}

func TestJitterStaysWithinTwentyPercent(t *testing.T) {
	tests := []struct {
		name string
		base time.Duration
	}{
		{"outbox sweep", 5 * time.Second},
		{"workflow execution", 2 * time.Second},
		{"scanner", 15 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			timer := New(tc.base, nil)
			lo, hi := tc.base*4/5, tc.base*6/5
			first := timer.next()
			if first < 0 || first >= tc.base {
				t.Fatalf("initial offset = %v, want [0, %v)", first, tc.base)
			}
			for range 10000 {
				if d := timer.next(); d < lo || d >= hi {
					t.Fatalf("sweep = %v, want [%v, %v)", d, lo, hi)
				}
			}
			if d := Jitter(tc.base); d < lo || d >= hi {
				t.Fatalf("Jitter = %v, want [%v, %v)", d, lo, hi)
			}
		})
	}
}

func TestLoopDrainsFullBatchWithoutWaiting(t *testing.T) {
	tests := []struct {
		name  string
		batch []bool
		want  int
	}{
		{"idle pass waits", []bool{false}, 1},
		{"full batch drains", []bool{true, true, false}, 3},
		{"partial batch stops", []bool{true, false, true}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timer := New(time.Hour, nil)
			wakes := 0
			timer.after = func(time.Duration) (<-chan time.Time, func()) {
				wakes++
				if wakes > 1 {
					cancel()
					return make(chan time.Time), func() {}
				}
				fired := make(chan time.Time, 1)
				fired <- time.Now()
				return fired, func() {}
			}
			calls := 0
			Loop(ctx, timer, func(context.Context) bool {
				full := tc.batch[calls]
				calls++
				return full
			})
			if calls != tc.want {
				t.Fatalf("passes = %d, want %d", calls, tc.want)
			}
		})
	}
}

// TestIdleSweepsPerMinute drives a sweep-only worker on virtual time and counts
// its passes over one idle minute. Each pass is one bounded query per tenant.
func TestIdleSweepsPerMinute(t *testing.T) {
	tests := []struct {
		name     string
		base     time.Duration
		maxPerMn int
		minPerMn int
	}{
		{"outbox and dispatchers at 5s", 5 * time.Second, 13, 11},
		{"workflow execution at 2s", 2 * time.Second, 31, 29},
		{"previous 1s ticker", time.Second, 61, 59},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timer := New(tc.base, nil)
			var virtual time.Duration
			timer.rnd = func(n time.Duration) time.Duration { return n / 2 }
			timer.after = func(d time.Duration) (<-chan time.Time, func()) {
				virtual += d
				if virtual >= time.Minute {
					cancel()
					return make(chan time.Time), func() {}
				}
				fired := make(chan time.Time, 1)
				fired <- time.Now()
				return fired, func() {}
			}
			passes := 0
			Loop(ctx, timer, func(context.Context) bool {
				passes++
				return false
			})
			if passes < tc.minPerMn || passes > tc.maxPerMn {
				t.Fatalf("idle passes per minute = %d, want [%d, %d]", passes, tc.minPerMn, tc.maxPerMn)
			}
		})
	}
}
