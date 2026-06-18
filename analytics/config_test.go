package analytics

import (
	"os"
	"testing"
)

func TestLoadConfig_Defaults(t *testing.T) {
	// Clear analytics env vars.
	os.Unsetenv("KORA_ANALYTICS")
	os.Unsetenv("KORA_ANALYTICS_CHANNEL_SIZE")
	os.Unsetenv("KORA_ANALYTICS_BATCH_SIZE")
	os.Unsetenv("KORA_ANALYTICS_FLUSH_INTERVAL")
	os.Unsetenv("KORA_ANALYTICS_RETENTION_DAYS")

	cfg := LoadConfig()

	if cfg.Enabled {
		t.Error("analytics should be disabled by default")
	}
	if cfg.ChannelSize != 1000 {
		t.Errorf("default channel size should be 1000, got %d", cfg.ChannelSize)
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

func TestLoadConfig_Enabled(t *testing.T) {
	os.Setenv("KORA_ANALYTICS", "true")
	defer os.Unsetenv("KORA_ANALYTICS")

	cfg := LoadConfig()
	if !cfg.Enabled {
		t.Error("analytics should be enabled when KORA_ANALYTICS=true")
	}
}

func TestLoadConfig_EnabledWith1(t *testing.T) {
	os.Setenv("KORA_ANALYTICS", "1")
	defer os.Unsetenv("KORA_ANALYTICS")

	cfg := LoadConfig()
	if !cfg.Enabled {
		t.Error("analytics should be enabled when KORA_ANALYTICS=1")
	}
}

func TestLoadConfig_CustomValues(t *testing.T) {
	os.Setenv("KORA_ANALYTICS", "true")
	os.Setenv("KORA_ANALYTICS_CHANNEL_SIZE", "500")
	os.Setenv("KORA_ANALYTICS_BATCH_SIZE", "50")
	os.Setenv("KORA_ANALYTICS_FLUSH_INTERVAL", "5s")
	os.Setenv("KORA_ANALYTICS_RETENTION_DAYS", "90")
	defer func() {
		for _, k := range []string{
			"KORA_ANALYTICS", "KORA_ANALYTICS_CHANNEL_SIZE",
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

func TestLoadConfig_RetentionZero(t *testing.T) {
	os.Setenv("KORA_ANALYTICS_RETENTION_DAYS", "0")
	defer os.Unsetenv("KORA_ANALYTICS_RETENTION_DAYS")

	cfg := LoadConfig()
	if cfg.RetentionDays != 0 {
		t.Errorf("retention should be 0 (indefinite), got %d", cfg.RetentionDays)
	}
}
