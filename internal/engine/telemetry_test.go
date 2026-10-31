package engine

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/site"
)

func TestTelemetryStartupFailureRollsBackAcquiredProvidersInReverseOrder(t *testing.T) {
	var closed []string
	startupErr := errors.New("metrics exporter unavailable")
	closeErr := errors.New("trace exporter shutdown failed")
	err := rollbackTelemetryProviders([]func(context.Context) error{
		func(context.Context) error { closed = append(closed, "trace"); return closeErr },
	}, startupErr)
	if !errors.Is(err, startupErr) || !errors.Is(err, closeErr) {
		t.Fatalf("rollback error = %v, want startup and cleanup errors", err)
	}
	if len(closed) != 1 || closed[0] != "trace" {
		t.Fatalf("shutdown order = %v, want [trace]", closed)
	}
}

func TestTelemetryRollbackUsesAcquisitionReverseOrder(t *testing.T) {
	var closed []string
	err := rollbackTelemetryProviders([]func(context.Context) error{
		func(context.Context) error { closed = append(closed, "trace"); return nil },
		func(context.Context) error { closed = append(closed, "metrics"); return nil },
	}, errors.New("later startup stage failed"))
	if err == nil {
		t.Fatal("rollback should preserve the startup error")
	}
	if len(closed) != 2 || closed[0] != "metrics" || closed[1] != "trace" {
		t.Fatalf("shutdown order = %v, want [metrics trace]", closed)
	}
}

func TestDirectoryLagMetricsExportOTLPHTTP(t *testing.T) {
	var path, contentType string
	var payload []byte
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		contentType = r.Header.Get("Content-Type")
		payload, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", collector.URL+"/v1/metrics")
	shutdown, err := InitializeTelemetry(context.Background(), TelemetryConfig{
		ServiceName: "kora-engine-test", ServiceVersion: "test", MetricsEnabled: true,
	})
	if err != nil {
		t.Fatal("initialize OTel metrics:", err)
	}
	metrics, err := NewDirectoryLagMetrics("cell-test")
	if err != nil {
		t.Fatal("create directory metric instruments:", err)
	}
	lag, age := uint64(3), 2*time.Second
	dsn := os.Getenv("KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN")
	consumerID := os.Getenv("KORA_SITE_DIRECTORY_LIVE_CONSUMER_ID")
	if dsn != "" && consumerID != "" {
		database, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatal("open live platform registry:", err)
		}
		defer database.Close()
		if err := database.Ping(); err != nil {
			t.Fatal("ping live platform registry:", err)
		}
		status, err := site.NewSQLSiteRegistry(database, "mysql").ConsumerLag(consumerID)
		if err != nil {
			t.Fatal("read live site-directory lag:", err)
		}
		lag, age = status.RevisionLag, status.OldestChangeAge
		t.Logf("live MySQL sample cursor=%d latest=%d revision_lag=%d stale_snapshot_age=%s", status.ConsumerRevision, status.LatestRevision, lag, age.Round(time.Millisecond))
	} else {
		t.Log("no live registry configured; exporting deterministic test sample")
	}
	metrics.Record(context.Background(), lag, age, true)
	flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(flushCtx); err != nil {
		t.Fatal("flush and shutdown OTel metrics:", err)
	}
	if path != "/v1/metrics" {
		t.Fatalf("OTLP metrics path = %q, want /v1/metrics", path)
	}
	if !strings.HasPrefix(contentType, "application/x-protobuf") || len(payload) == 0 {
		t.Fatalf("OTLP request content type=%q payload bytes=%d", contentType, len(payload))
	}
}
