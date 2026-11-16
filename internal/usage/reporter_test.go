package usage

import (
	"context"
	"encoding/json"
	"errors"
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
	tests := []struct {
		name    string
		count   func(context.Context) (int64, error)
		status  int
		wantErr bool
		want    *sentRecord
	}{
		{name: "posts the record count", count: func(context.Context) (int64, error) { return 1234, nil }, status: http.StatusAccepted,
			want: &sentRecord{ID: "site-1-records-1794830400", SiteID: "site-1", Metric: "records", Quantity: 1234,
				WindowStart: end.Add(-6 * time.Hour), WindowEnd: end, Labels: map[string]string{"source": "engine"}}},
		{name: "count fails, nothing sent", count: func(context.Context) (int64, error) { return 0, errors.New("db down") }, wantErr: true},
		{name: "Cloud refuses", count: func(context.Context) (int64, error) { return 1, nil }, status: http.StatusUnauthorized, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *sentRecord
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("request = %s auth %q type %q", r.Method, r.Header.Get("Authorization"), r.Header.Get("Content-Type"))
				}
				got = &sentRecord{}
				if err := json.NewDecoder(r.Body).Decode(got); err != nil {
					t.Errorf("decode: %v", err)
				}
				w.WriteHeader(tt.status)
			}))
			defer server.Close()
			reporter := &Reporter{Config: Config{URL: server.URL, Token: "secret", Interval: 6 * time.Hour}, SiteID: "site-1", Count: tt.count,
				Now: func() time.Time { return end }}
			err := reporter.Report(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.want == nil {
				return
			}
			if got == nil || got.ID != tt.want.ID || got.SiteID != tt.want.SiteID || got.Metric != tt.want.Metric || got.Quantity != tt.want.Quantity ||
				!got.WindowStart.Equal(tt.want.WindowStart) || !got.WindowEnd.Equal(tt.want.WindowEnd) || got.Labels["source"] != "engine" {
				t.Fatalf("sent %+v, want %+v", got, tt.want)
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
		Count: func(context.Context) (int64, error) { return 1, nil }}
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
