package analytics

import (
	"os"
	"testing"
	"time"
)

func TestLoadConfig_Defaults(t *testing.T) {
	os.Unsetenv("KORA_ANALYTICS_CHANNEL_SIZE")
	os.Unsetenv("KORA_ANALYTICS_MAX_EVENT_BYTES")
	os.Unsetenv("KORA_ANALYTICS_MAX_QUEUED_BYTES")
	os.Unsetenv("KORA_ANALYTICS_BATCH_SIZE")
	os.Unsetenv("KORA_ANALYTICS_FLUSH_INTERVAL")
	os.Unsetenv("KORA_ANALYTICS_RETENTION_DAYS")

	cfg := LoadConfig()

	if cfg.ChannelSize != 1000 {
		t.Errorf("default channel size should be 1000, got %d", cfg.ChannelSize)
	}
	if cfg.MaxEventBytes != DefaultMaxEventBytes {
		t.Errorf("default max event size = %d, want %d", cfg.MaxEventBytes, DefaultMaxEventBytes)
	}
	if cfg.MaxQueuedBytes != DefaultMaxQueuedBytes {
		t.Errorf("default max queued bytes = %d, want %d", cfg.MaxQueuedBytes, DefaultMaxQueuedBytes)
	}
	if cfg.BatchSize != 100 {
		t.Errorf("default batch size should be 100, got %d", cfg.BatchSize)
	}
	if cfg.FlushInterval != "1s" {
		t.Errorf("default flush interval should be '1s', got %q", cfg.FlushInterval)
	}
	if cfg.RetentionDays != 30 {
		t.Errorf("default retention should be 30 days, got %d", cfg.RetentionDays)
	}
}

func TestRetentionCutoff(t *testing.T) {
	now := time.Date(2026, time.October, 3, 0, 0, 0, 0, time.UTC)
	cutoff, enabled := retentionCutoff(now, 90)
	if !enabled || cutoff != "2026-07-05" {
		t.Fatalf("retentionCutoff(90) = (%q, %t), want (2026-07-05, true)", cutoff, enabled)
	}
	cutoff, enabled = retentionCutoff(now, 0)
	if enabled || cutoff != "" {
		t.Fatalf("retentionCutoff(0) = (%q, %t), want empty and disabled", cutoff, enabled)
	}
}

func TestLoadConfig_CustomValues(t *testing.T) {
	os.Setenv("KORA_ANALYTICS_CHANNEL_SIZE", "500")
	os.Setenv("KORA_ANALYTICS_MAX_EVENT_BYTES", "2048")
	os.Setenv("KORA_ANALYTICS_MAX_QUEUED_BYTES", "8192")
	os.Setenv("KORA_ANALYTICS_BATCH_SIZE", "50")
	os.Setenv("KORA_ANALYTICS_FLUSH_INTERVAL", "5s")
	os.Setenv("KORA_ANALYTICS_RETENTION_DAYS", "90")
	defer func() {
		for _, k := range []string{
			"KORA_ANALYTICS_CHANNEL_SIZE",
			"KORA_ANALYTICS_MAX_EVENT_BYTES",
			"KORA_ANALYTICS_MAX_QUEUED_BYTES",
			"KORA_ANALYTICS_BATCH_SIZE", "KORA_ANALYTICS_FLUSH_INTERVAL",
			"KORA_ANALYTICS_RETENTION_DAYS",
		} {
			os.Unsetenv(k)
		}
	}()

	cfg := LoadConfig()

	if cfg.ChannelSize != 500 {
		t.Errorf("channel size = %d, want 500", cfg.ChannelSize)
	}
	if cfg.MaxEventBytes != 2048 {
		t.Errorf("max event size = %d, want 2048", cfg.MaxEventBytes)
	}
	if cfg.MaxQueuedBytes != 8192 {
		t.Errorf("max queued bytes = %d, want 8192", cfg.MaxQueuedBytes)
	}
	if cfg.BatchSize != 50 {
		t.Errorf("batch size = %d, want 50", cfg.BatchSize)
	}
	if cfg.FlushInterval != "5s" {
		t.Errorf("flush interval = %q, want '5s'", cfg.FlushInterval)
	}
	if cfg.RetentionDays != 90 {
		t.Errorf("retention = %d, want 90", cfg.RetentionDays)
	}
}

func TestLoadConfigCapsAnalyticsBatchSize(t *testing.T) {
	t.Setenv("KORA_ANALYTICS_BATCH_SIZE", "999999999")
	cfg := LoadConfig()
	if cfg.BatchSize != MaxAnalyticsBatchSize {
		t.Fatalf("batch size = %d, want hard maximum %d", cfg.BatchSize, MaxAnalyticsBatchSize)
	}
}

func TestLoadConfig_RetentionZero(t *testing.T) {
	os.Setenv("KORA_ANALYTICS_RETENTION_DAYS", "0")
	defer os.Unsetenv("KORA_ANALYTICS_RETENTION_DAYS")

	cfg := LoadConfig()
	if cfg.RetentionDays != 0 {
		t.Errorf("retention should be 0 (indefinite), got %d", cfg.RetentionDays)
	}
}
