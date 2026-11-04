package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/asenawritescode/kora/outbox"
)

func TestRecordOutboxPublishErrorCountsOnlyFencedLeaseLoss(t *testing.T) {
	metrics := &outbox.BacklogMetrics{}
	recordOutboxPublishError(metrics, errors.New("temporary provider failure"))
	if got := metrics.Snapshot(time.Now()).LeaseLost; got != 0 {
		t.Fatalf("lease losses after provider error = %d, want 0", got)
	}
	recordOutboxPublishError(metrics, errors.Join(errors.New("publish batch failed"), outbox.ErrLeaseLost))
	if got := metrics.Snapshot(time.Now()).LeaseLost; got != 1 {
		t.Fatalf("lease losses after fenced claim = %d, want 1", got)
	}
}
