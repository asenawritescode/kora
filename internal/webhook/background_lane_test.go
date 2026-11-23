package webhook

import (
	"context"
	"errors"
	"testing"

	"github.com/asenawritescode/kora/internal/dbaccess"
)

type countingAdmission struct {
	calls int
	err   error
}

func (a *countingAdmission) Acquire(context.Context) (func(), error) {
	a.calls++
	if a.err != nil {
		return nil, a.err
	}
	return func() {}, nil
}

func TestOutboxDBWorkUsesBackgroundLaneAndSkipsBusyWithoutRetry(t *testing.T) {
	tests := []struct {
		name           string
		outbox         bool
		busy           bool
		wantRequest    int
		wantBackground int
		wantBusy       bool
	}{
		{name: "request path keeps ordinary lane", wantRequest: 1},
		{name: "outbox path draws on background lane", outbox: true, wantBackground: 1},
		{name: "busy background pass is returned once", outbox: true, busy: true, wantBackground: 1, wantBusy: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request, background := &countingAdmission{}, &countingAdmission{}
			if tt.busy {
				background.err = dbaccess.ErrBackgroundBusy
			}
			worker := &Worker{WorkAdmission: request, BackgroundAdmission: background}
			// A done context models the pass deadline a busy lane waits for.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if tt.outbox {
				ctx = context.WithValue(ctx, outboxEventKey{}, true)
			}
			release, err := worker.acquireDBWork(ctx)
			if got := dbaccess.IsBackgroundBusy(err); got != tt.wantBusy {
				t.Fatalf("busy = %v (err %v), want %v", got, err, tt.wantBusy)
			}
			if err == nil {
				release()
			}
			if request.calls != tt.wantRequest || background.calls != tt.wantBackground {
				t.Fatalf("request calls=%d background calls=%d, want %d/%d", request.calls, background.calls, tt.wantRequest, tt.wantBackground)
			}
		})
	}
}

func TestOutboxDBWorkReturnsContextErrorWithoutBackgroundLane(t *testing.T) {
	request := &countingAdmission{err: errors.New("lane unavailable")}
	worker := &Worker{WorkAdmission: request}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := worker.acquireDBWork(ctx); err == nil {
		t.Fatal("canceled work acquired a lane")
	}
}
