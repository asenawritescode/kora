package webhook

import "testing"

func TestWorkerStopIsIdempotent(t *testing.T) {
	worker := NewWorker(nil, nil, "test")
	worker.Stop()
	worker.Stop()
}
