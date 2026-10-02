package cli

import (
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestSiteLoadFailureTraceDoesNotContainDriverCredentials(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	tracer := provider.Tracer("kora/test")
	_, span := tracer.Start(t.Context(), "engine.site.connect")
	span.SetAttributes(attribute.String("db.system", "postgres"))
	endPhase(span, errors.New("connect postgres://tenant-user:sentinel-password@db.internal/site: refused"))

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected one ended span, got %d", len(spans))
	}
	ended := spans[0]
	if ended.Status().Description != "site initialization phase failed" {
		t.Fatalf("unexpected status description: %q", ended.Status().Description)
	}
	var material strings.Builder
	material.WriteString(ended.Status().Description)
	for _, attr := range ended.Attributes() {
		material.WriteString(string(attr.Key))
		material.WriteString(attr.Value.Emit())
	}
	for _, event := range ended.Events() {
		material.WriteString(event.Name)
		for _, attr := range event.Attributes {
			material.WriteString(string(attr.Key))
			material.WriteString(attr.Value.Emit())
		}
	}
	if strings.Contains(material.String(), "sentinel-password") || strings.Contains(material.String(), "tenant-user") {
		t.Fatalf("trace contains connection credentials: %s", material.String())
	}
	if err := provider.Shutdown(t.Context()); err != nil {
		t.Fatal("shutdown test tracer provider:", err)
	}
}
