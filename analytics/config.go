package analytics

import (
	"os"
	"strconv"
)

// Config holds analytics engine configuration. All values have sensible defaults;
// analytics is disabled when Enabled is false.
type Config struct {
	Enabled       bool   // true when KORA_ANALYTICS=true
	ChannelSize   int    // event bus buffer capacity (default 1000)
	BatchSize     int    // flush worker deltas after N events (default 100)
	FlushInterval string // flush worker deltas after duration (default "1s")
	RetentionDays int    // how long to keep daily rollup rows (default 30; 0 = indefinite)
	WALDir        string // write-ahead log for spilled events (default "data/analytics/wal")
}

// LoadConfig reads analytics configuration from environment variables.
func LoadConfig() *Config {
	cfg := &Config{
		Enabled:       false,
		ChannelSize:   1000,
		BatchSize:     100,
		FlushInterval: "1s",
		RetentionDays: 30,
		WALDir:        "data/analytics/wal",
	}

	if v := os.Getenv("KORA_ANALYTICS"); v == "true" || v == "1" {
		cfg.Enabled = true
	}

	if v := os.Getenv("KORA_ANALYTICS_CHANNEL_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.ChannelSize = n
		}
	}

	if v := os.Getenv("KORA_ANALYTICS_BATCH_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.BatchSize = n
		}
	}

	if v := os.Getenv("KORA_ANALYTICS_FLUSH_INTERVAL"); v != "" {
		cfg.FlushInterval = v
	}

	if v := os.Getenv("KORA_ANALYTICS_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.RetentionDays = n
		}
	}

	if v := os.Getenv("KORA_ANALYTICS_WAL_DIR"); v != "" {
		cfg.WALDir = v
	}

	return cfg
}
