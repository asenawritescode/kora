package engine

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/asenawritescode/kora/site"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// DirectoryLagMetrics instruments the durable site-directory consumer cursor.
type DirectoryLagMetrics struct {
	revisionLag metric.Int64Gauge
	staleAge    metric.Float64Gauge
	observed    metric.Int64Gauge
	attributes  []attribute.KeyValue
}

func NewDirectoryLagMetrics(cellID string) (*DirectoryLagMetrics, error) {
	meter := otel.Meter("kora/engine/site-directory")
	revisionLag, err := meter.Int64Gauge("kora.site_directory.revision_lag", metric.WithUnit("{revision}"), metric.WithDescription("Latest platform directory revision minus the replica's applied revision"))
	if err != nil {
		return nil, err
	}
	staleAge, err := meter.Float64Gauge("kora.site_directory.stale_snapshot_age", metric.WithUnit("s"), metric.WithDescription("Age of the oldest unapplied directory change; zero means the consumer is current"))
	if err != nil {
		return nil, err
	}
	observed, err := meter.Int64Gauge("kora.site_directory.observation_success", metric.WithUnit("{observation}"), metric.WithDescription("Whether the latest platform directory lag observation succeeded"))
	if err != nil {
		return nil, err
	}
	return &DirectoryLagMetrics{
		revisionLag: revisionLag,
		staleAge:    staleAge,
		observed:    observed,
		attributes:  []attribute.KeyValue{attribute.String("engine.cell_id", cellID)},
	}, nil
}

func (m *DirectoryLagMetrics) Record(ctx context.Context, lag uint64, staleAge time.Duration, success bool) {
	if m == nil {
		return
	}
	observed := int64(0)
	if success {
		observed = 1
	}
	m.observed.Record(ctx, observed, metric.WithAttributes(m.attributes...))
	m.revisionLag.Record(ctx, int64(lag), metric.WithAttributes(m.attributes...))
	m.staleAge.Record(ctx, staleAge.Seconds(), metric.WithAttributes(m.attributes...))
}

// DirectoryLagMonitor owns the process poller for a directory consumer.
type DirectoryLagMonitor struct {
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	startMu  sync.Mutex
	started  bool
	doneOnce sync.Once
	observe  func(context.Context)
	interval time.Duration
}

type directoryWorkAdmission interface {
	Acquire(context.Context) (func(), error)
}

// StartDirectoryLagMonitor starts and owns the periodic directory-lag poller.
func StartDirectoryLagMonitor(parent context.Context, registry *site.SQLSiteRegistry, consumerID string, metrics *DirectoryLagMetrics) (*DirectoryLagMonitor, error) {
	monitor, err := PrepareDirectoryLagMonitor(parent, registry, consumerID, metrics)
	if err != nil {
		return nil, err
	}
	if err := monitor.Start(); err != nil {
		return nil, err
	}
	return monitor, nil
}

// PrepareDirectoryLagMonitor constructs the owned poller without starting its
// goroutine; Engine.Run activates it through the process worker registry.
func PrepareDirectoryLagMonitor(parent context.Context, registry *site.SQLSiteRegistry, consumerID string, metrics *DirectoryLagMetrics, admissions ...directoryWorkAdmission) (*DirectoryLagMonitor, error) {
	if parent == nil {
		return nil, errors.New("directory lag monitor context is required")
	}
	if registry == nil || consumerID == "" || metrics == nil {
		return nil, errors.New("directory lag monitor registry, consumer ID and metrics are required")
	}
	var admission directoryWorkAdmission
	if len(admissions) > 0 {
		admission = admissions[0]
	}
	return prepareDirectoryLagMonitor(parent, func(ctx context.Context) {
		observeDirectoryLag(ctx, registry, consumerID, metrics, admission)
	}, 5*time.Second)
}

func observeDirectoryLag(ctx context.Context, registry *site.SQLSiteRegistry, consumerID string, metrics *DirectoryLagMetrics, admission directoryWorkAdmission) {
	if admission != nil {
		release, err := admission.Acquire(ctx)
		if err != nil {
			metrics.Record(ctx, 0, 0, false)
			return
		}
		defer release()
	}
	status, err := registry.ConsumerLagContext(ctx, consumerID)
	if err != nil {
		metrics.Record(ctx, 0, 0, false)
		return
	}
	metrics.Record(ctx, status.RevisionLag, status.OldestChangeAge, true)
}

func startDirectoryLagMonitor(parent context.Context, observe func(context.Context), interval time.Duration) (*DirectoryLagMonitor, error) {
	monitor, err := prepareDirectoryLagMonitor(parent, observe, interval)
	if err != nil {
		return nil, err
	}
	if err := monitor.Start(); err != nil {
		return nil, err
	}
	return monitor, nil
}

func prepareDirectoryLagMonitor(parent context.Context, observe func(context.Context), interval time.Duration) (*DirectoryLagMonitor, error) {
	if parent == nil || observe == nil || interval <= 0 {
		return nil, errors.New("directory lag monitor requires context, observer and positive interval")
	}
	ctx, cancel := context.WithCancel(parent)
	return &DirectoryLagMonitor{ctx: ctx, cancel: cancel, done: make(chan struct{}), observe: observe, interval: interval}, nil
}

// Start begins the monitor after the process owner activates its workers.
func (m *DirectoryLagMonitor) Start() error {
	if m == nil {
		return errors.New("directory lag monitor is required")
	}
	m.startMu.Lock()
	defer m.startMu.Unlock()
	if m.started {
		return nil
	}
	m.started = true
	go func() {
		defer m.doneOnce.Do(func() { close(m.done) })
		m.observe(m.ctx)
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-ticker.C:
				m.observe(m.ctx)
			}
		}
	}()
	return nil
}

// Shutdown cancels and joins the monitor before its directory DB is closed.
func (m *DirectoryLagMonitor) Shutdown(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.cancel()
	m.startMu.Lock()
	if !m.started {
		m.doneOnce.Do(func() { close(m.done) })
	}
	m.startMu.Unlock()
	return waitForWorker(ctx, "tenant directory lag monitor", m.done)
}
