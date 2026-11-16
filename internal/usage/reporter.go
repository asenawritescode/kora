package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Metric names Cloud's usage endpoint receives.
const (
	MetricRecords      = "records"
	MetricStorageBytes = "storage_bytes"
)

const defaultFirstDelay = time.Minute

// Meter measures one usage metric of a tenant.
type Meter struct {
	Metric  string
	Measure func(context.Context) (int64, error)
}

// Reporter posts one tenant's usage snapshots to Cloud every Config.Interval.
type Reporter struct {
	Config Config
	// SiteID is the tenant's site ID, which Cloud uses as its workspace ID.
	SiteID string
	// Meters are measured and posted on every report, one snapshot each.
	Meters []Meter
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

// Report measures every meter and posts one snapshot per meter, all covering
// the interval that ends now. A meter that fails does not stop the others; the
// errors are joined.
func (r *Reporter) Report(ctx context.Context) error {
	end := r.now().UTC().Truncate(time.Second)
	var errs []error
	for _, meter := range r.Meters {
		if err := r.reportMeter(ctx, meter, end); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *Reporter) reportMeter(ctx context.Context, meter Meter, end time.Time) error {
	quantity, err := meter.Measure(ctx)
	if err != nil {
		return fmt.Errorf("measure %s: %w", meter.Metric, err)
	}
	body, err := json.Marshal(snapshot{
		ID: fmt.Sprintf("%s-%s-%d", r.SiteID, meter.Metric, end.Unix()), SiteID: r.SiteID, Metric: meter.Metric, Quantity: quantity,
		WindowStart: end.Add(-r.Config.Interval), WindowEnd: end, Labels: map[string]string{"source": "engine"},
	})
	if err != nil {
		return fmt.Errorf("encode %s snapshot: %w", meter.Metric, err)
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
