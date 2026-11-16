package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type sentRecord struct {
	ID          string            `json:"id"`
	SiteID      string            `json:"site_id"`
	Metric      string            `json:"metric"`
	Quantity    int64             `json:"quantity"`
	WindowStart time.Time         `json:"window_start"`
	WindowEnd   time.Time         `json:"window_end"`
	Labels      map[string]string `json:"labels"`
}

func TestReport(t *testing.T) {
	end := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	records := func(n int64) Meter {
		return Meter{Metric: MetricRecords, Measure: func(context.Context) (int64, error) { return n, nil }}
	}
	storage := func(n int64) Meter {
		return Meter{Metric: MetricStorageBytes, Measure: func(context.Context) (int64, error) { return n, nil }}
	}
	failing := func(metric string) Meter {
		return Meter{Metric: metric, Measure: func(context.Context) (int64, error) { return 0, errors.New("db down") }}
	}
	tests := []struct {
		name    string
		meters  []Meter
		status  int
		wantErr bool
		// want lists the snapshots Cloud receives, in order.
		want []sentRecord
	}{
		{name: "posts record count and storage bytes", meters: []Meter{records(1234), storage(4096)}, status: http.StatusAccepted,
			want: []sentRecord{
				{ID: fmt.Sprintf("site-1-records-%d", end.Unix()), Metric: "records", Quantity: 1234},
				{ID: fmt.Sprintf("site-1-storage_bytes-%d", end.Unix()), Metric: "storage_bytes", Quantity: 4096},
			}},
		{name: "record count fails, storage still posted", meters: []Meter{failing(MetricRecords), storage(4096)}, status: http.StatusAccepted, wantErr: true,
			want: []sentRecord{{ID: fmt.Sprintf("site-1-storage_bytes-%d", end.Unix()), Metric: "storage_bytes", Quantity: 4096}}},
		{name: "storage fails, record count still posted", meters: []Meter{records(7), failing(MetricStorageBytes)}, status: http.StatusAccepted, wantErr: true,
			want: []sentRecord{{ID: fmt.Sprintf("site-1-records-%d", end.Unix()), Metric: "records", Quantity: 7}}},
		{name: "every meter fails, nothing sent", meters: []Meter{failing(MetricRecords), failing(MetricStorageBytes)}, wantErr: true},
		{name: "Cloud refuses both snapshots", meters: []Meter{records(1), storage(2)}, status: http.StatusUnauthorized, wantErr: true,
			want: []sentRecord{
				{ID: fmt.Sprintf("site-1-records-%d", end.Unix()), Metric: "records", Quantity: 1},
				{ID: fmt.Sprintf("site-1-storage_bytes-%d", end.Unix()), Metric: "storage_bytes", Quantity: 2},
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []sentRecord
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("request = %s auth %q type %q", r.Method, r.Header.Get("Authorization"), r.Header.Get("Content-Type"))
				}
				var rec sentRecord
				if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
					t.Errorf("decode: %v", err)
				}
				got = append(got, rec)
				w.WriteHeader(tt.status)
			}))
			defer server.Close()
			reporter := &Reporter{Config: Config{URL: server.URL, Token: "secret", Interval: 6 * time.Hour}, SiteID: "site-1", Meters: tt.meters,
				Now: func() time.Time { return end }}
			err := reporter.Report(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("sent %d snapshots, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, want := range tt.want {
				g := got[i]
				if g.ID != want.ID || g.SiteID != "site-1" || g.Metric != want.Metric || g.Quantity != want.Quantity ||
					!g.WindowStart.Equal(end.Add(-6*time.Hour)) || !g.WindowEnd.Equal(end) || g.Labels["source"] != "engine" {
					t.Errorf("snapshot %d = %+v, want %+v", i, g, want)
				}
			}
		})
	}
}

func TestRunReportsUntilStopped(t *testing.T) {
	var sent atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sent.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	reporter := &Reporter{Config: Config{URL: server.URL, Token: "t", Interval: time.Millisecond}, SiteID: "site-1", FirstDelay: time.Millisecond,
		Meters: []Meter{{Metric: MetricRecords, Measure: func(context.Context) (int64, error) { return 1, nil }}}}
	done := make(chan struct{})
	go func() { reporter.Run(ctx); close(done) }()
	deadline := time.After(5 * time.Second)
	for sent.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("reporter did not repeat")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}
