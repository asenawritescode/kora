package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

const (
	metricRecords     = "records"
	defaultFirstDelay = time.Minute
)

// Reporter posts one tenant's usage snapshot to Cloud every Config.Interval.
type Reporter struct {
	Config Config
	// SiteID is the tenant's site ID, which Cloud uses as its workspace ID.
	SiteID string
	// Count returns the tenant's current record count.
	Count func(context.Context) (int64, error)
	// FirstDelay is the wait before the first report (default one minute),
	// so a restart does not report every tenant at once.
	FirstDelay time.Duration
	Client     *http.Client
	Now        func() time.Time
}

// snapshot is the body of POST /api/cloud/usage (Cloud's domain.UsageRecord).
type snapshot struct {
	ID          string            `json:"id"`
	SiteID      string            `json:"site_id"`
	Metric      string            `json:"metric"`
	Quantity    int64             `json:"quantity"`
	WindowStart time.Time         `json:"window_start"`
	WindowEnd   time.Time         `json:"window_end"`
	Labels      map[string]string `json:"labels"`
}

// Run reports until ctx ends. A failed report is logged and retried at the
// next interval; it never stops the tenant.
func (r *Reporter) Run(ctx context.Context) {
	delay := r.FirstDelay
	if delay <= 0 {
		delay = defaultFirstDelay
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := r.Report(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("cloud usage report failed", "site_id", r.SiteID, "error", err)
		}
		timer.Reset(r.Config.Interval)
	}
}

// Report counts the tenant's records and posts one snapshot covering the
// interval that ends now.
func (r *Reporter) Report(ctx context.Context) error {
	count, err := r.Count(ctx)
	if err != nil {
		return fmt.Errorf("count records: %w", err)
	}
	end := r.now().UTC().Truncate(time.Second)
	body, err := json.Marshal(snapshot{
		ID: fmt.Sprintf("%s-%s-%d", r.SiteID, metricRecords, end.Unix()), SiteID: r.SiteID, Metric: metricRecords, Quantity: count,
		WindowStart: end.Add(-r.Config.Interval), WindowEnd: end, Labels: map[string]string{"source": "engine"},
	})
	if err != nil {
		return fmt.Errorf("encode usage snapshot: %w", err)
	}
	return r.post(ctx, body)
}

func (r *Reporter) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Config.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build usage request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.Config.Token)
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send usage snapshot: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // response body is not read
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("cloud usage endpoint answered %d", resp.StatusCode)
	}
	return nil
}

func (r *Reporter) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
