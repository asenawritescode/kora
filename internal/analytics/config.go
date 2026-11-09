package analytics

import (
	"os"
	"strconv"
	"time"
)

const (
	DefaultAnalyticsBatchSize = 100
	MaxAnalyticsBatchSize     = 500
)

// Config holds analytics engine configuration. All values have sensible defaults.
type Config struct {
	ChannelSize    int    // event bus buffer capacity (default 1000)
	MaxEventBytes  int    // maximum encoded event size (default 1 MiB)
	MaxQueuedBytes int    // maximum serialized-payload budget for the in-memory wake-up queue (default 64 MiB)
	BatchSize      int    // flush worker deltas after N events (default 100)
	FlushInterval  string // flush worker deltas after duration (default "1s")
	RetentionDays  int    // how long to keep daily rollup rows (default 30; 0 = indefinite)
	WALDir         string // write-ahead log for spilled events (default "data/analytics/wal")
}

// LoadConfig reads analytics configuration from environment variables.
func LoadConfig() *Config {
	cfg := &Config{
		ChannelSize:    1000,
		MaxEventBytes:  DefaultMaxEventBytes,
		MaxQueuedBytes: DefaultMaxQueuedBytes,
		BatchSize:      DefaultAnalyticsBatchSize,
		FlushInterval:  "1s",
		RetentionDays:  30,
		WALDir:         "data/analytics/wal",
	}

	if v := os.Getenv("KORA_ANALYTICS_CHANNEL_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= MaxChannelCapacity {
			cfg.ChannelSize = n
		}
	}

	if v := os.Getenv("KORA_ANALYTICS_MAX_EVENT_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 16<<20 {
			cfg.MaxEventBytes = n
		}
	}

	if v := os.Getenv("KORA_ANALYTICS_MAX_QUEUED_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= MaxConfiguredQueuedBytes {
			cfg.MaxQueuedBytes = n
		}
	}
	if cfg.MaxEventBytes > cfg.MaxQueuedBytes {
		cfg.MaxEventBytes = cfg.MaxQueuedBytes
	}

	if v := os.Getenv("KORA_ANALYTICS_BATCH_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > MaxAnalyticsBatchSize {
				n = MaxAnalyticsBatchSize
			}
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

func retentionCutoff(now time.Time, days int) (string, bool) {
	if days <= 0 {
		return "", false
	}
	return now.AddDate(0, 0, -days).Format("2006-01-02"), true
}
