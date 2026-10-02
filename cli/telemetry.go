package cli

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/asenawritescode/kora/site"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type directoryLagMetrics struct {
	revisionLag metric.Int64Gauge
	staleAge    metric.Float64Gauge
	observed    metric.Int64Gauge
	attributes  []attribute.KeyValue
}

func newDirectoryLagMetrics(cellID string) (*directoryLagMetrics, error) {
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
	return &directoryLagMetrics{
		revisionLag: revisionLag,
		staleAge:    staleAge,
		observed:    observed,
		attributes: []attribute.KeyValue{
			attribute.String("engine.cell_id", cellID),
		},
	}, nil
}

func (m *directoryLagMetrics) record(ctx context.Context, lag uint64, staleAge time.Duration, success bool) {
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

func monitorDirectoryLag(ctx context.Context, registry *site.SQLSiteRegistry, consumerID string, metrics *directoryLagMetrics) {
	observe := func() {
		status, err := registry.ConsumerLag(consumerID)
		if err != nil {
			metrics.record(ctx, 0, 0, false)
			return
		}
		metrics.record(ctx, status.RevisionLag, status.OldestChangeAge, true)
	}
	observe()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			observe()
		}
	}
}

func initOpenTelemetry(ctx context.Context) (func(context.Context) error, error) {
	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "kora-engine"
	}
	serviceResource := resource.NewSchemaless(
		attribute.String("service.name", serviceName),
		attribute.String("service.version", Version),
	)
	var shutdowns []func(context.Context) error
	traceEnabled := !strings.EqualFold(os.Getenv("OTEL_TRACES_EXPORTER"), "none") &&
		(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "")
	if traceEnabled {
		exporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, err
		}
		provider := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exporter),
			sdktrace.WithResource(serviceResource),
		)
		otel.SetTracerProvider(provider)
		shutdowns = append(shutdowns, provider.Shutdown)
	}

	// Keep metrics opt-in and endpoint-specific: Jaeger commonly receives traces
	// but does not ingest OTLP metrics. This avoids silently sending metrics to a
	// trace-only endpoint while allowing standard OTLP collectors to receive both.
	metricsEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT")
	if metricsEndpoint != "" && !strings.EqualFold(os.Getenv("OTEL_METRICS_EXPORTER"), "none") {
		exporter, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return nil, err
		}
		provider := sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(10*time.Second))),
			sdkmetric.WithResource(serviceResource),
		)
		otel.SetMeterProvider(provider)
		shutdowns = append(shutdowns, provider.Shutdown)
	}
	return func(ctx context.Context) error {
		var shutdownErr error
		for index := len(shutdowns) - 1; index >= 0; index-- {
			if err := shutdowns[index](ctx); err != nil && shutdownErr == nil {
				shutdownErr = err
			}
		}
		return shutdownErr
	}, nil
}
