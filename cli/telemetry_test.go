package cli

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/site"
)

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

	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", collector.URL+"/v1/metrics")
	shutdown, err := initOpenTelemetry(context.Background())
	if err != nil {
		t.Fatal("initialize OTel metrics:", err)
	}
	metrics, err := newDirectoryLagMetrics("cell-test")
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
	metrics.record(context.Background(), lag, age, true)
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
