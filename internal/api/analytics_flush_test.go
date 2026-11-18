package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// recordingAnalyticsWorker stands in for the site analytics worker. Its
// RequestFlush only records the call, so a handler that waited on a flush
// would show up as a blocked helper, not as a slow request.
type recordingAnalyticsWorker struct{ requests int }

func (w *recordingAnalyticsWorker) InvalidateMetrics(string) {}
func (w *recordingAnalyticsWorker) InvalidateAllMetrics()    {}
func (w *recordingAnalyticsWorker) RequestFlush()            { w.requests++ }

func TestAfterAnalyticsWriteRequestsFlushWithoutWaiting(t *testing.T) {
	for _, tc := range []struct {
		name         string
		worker       analyticsWorker
		wantRequests int
	}{
		{name: "no analytics worker", worker: nil, wantRequests: 0},
		{name: "worker receives one flush request", worker: &recordingAnalyticsWorker{}, wantRequests: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			if tc.worker != nil {
				c.Set("site_analytics_worker", tc.worker)
			}
			done := make(chan struct{})
			go func() {
				(&Handler{}).afterAnalyticsWrite(c, "TestDoc")
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("afterAnalyticsWrite waited on the analytics worker")
			}
			if tc.worker == nil {
				return
			}
			if got := tc.worker.(*recordingAnalyticsWorker).requests; got != tc.wantRequests {
				t.Fatalf("RequestFlush calls = %d, want %d", got, tc.wantRequests)
			}
		})
	}
}
