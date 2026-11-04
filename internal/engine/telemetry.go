package engine

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TelemetryConfig selects process telemetry providers. Endpoint and exporter
// protocol details remain controlled by the standard OTEL environment options.
type TelemetryConfig struct {
	ServiceName    string
	ServiceVersion string
	TracesEnabled  bool
	MetricsEnabled bool
}

// InitializeTelemetry constructs exporters without publishing partial global
// providers. The returned shutdown function is owned by ProcessResources.
func InitializeTelemetry(ctx context.Context, config TelemetryConfig) (func(context.Context) error, error) {
	if ctx == nil {
		return nil, errors.New("telemetry initialization context is required")
	}
	serviceName := strings.TrimSpace(config.ServiceName)
	if serviceName == "" {
		serviceName = "kora-engine"
	}
	serviceResource := resource.NewSchemaless(
		attribute.String("service.name", serviceName),
		attribute.String("service.version", config.ServiceVersion),
	)
	var shutdowns []func(context.Context) error
	var traceProvider *sdktrace.TracerProvider
	var metricsProvider *sdkmetric.MeterProvider
	if config.TracesEnabled {
		exporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, err
		}
		traceProvider = sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exporter),
			sdktrace.WithResource(serviceResource),
		)
		shutdowns = append(shutdowns, traceProvider.Shutdown)
	}
	if config.MetricsEnabled {
		exporter, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return nil, rollbackTelemetryProviders(shutdowns, err)
		}
		metricsProvider = sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(10*time.Second))),
			sdkmetric.WithResource(serviceResource),
		)
		shutdowns = append(shutdowns, metricsProvider.Shutdown)
	}
	if traceProvider != nil {
		otel.SetTracerProvider(traceProvider)
	}
	if metricsProvider != nil {
		otel.SetMeterProvider(metricsProvider)
	}
	return func(ctx context.Context) error {
		var shutdownErr error
		for index := len(shutdowns) - 1; index >= 0; index-- {
			shutdownErr = errors.Join(shutdownErr, shutdowns[index](ctx))
		}
		return shutdownErr
	}, nil
}

func rollbackTelemetryProviders(shutdowns []func(context.Context) error, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	joined := cause
	for index := len(shutdowns) - 1; index >= 0; index-- {
		joined = errors.Join(joined, shutdowns[index](ctx))
	}
	return joined
}
