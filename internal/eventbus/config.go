package eventbus

import (
	"os"
	"strconv"
)

// LegacyWALDir is where the retired analytics write-ahead log lived. Nothing
// writes it any more; startup only reports whether it still holds files.
const LegacyWALDir = "data/analytics/wal"

// Config bounds the in-process change-event bus of each tenant.
type Config struct {
	ChannelSize   int // wake-up channel capacity (default 1000)
	MaxEventBytes int // maximum encoded event size (default 1 MiB)
}

// LoadConfig reads KORA_ANALYTICS_CHANNEL_SIZE and KORA_ANALYTICS_MAX_EVENT_BYTES.
// Invalid or out-of-range values keep the default.
func LoadConfig() *Config {
	cfg := &Config{ChannelSize: 1000, MaxEventBytes: DefaultMaxEventBytes}
	if n, ok := positiveEnvInt("KORA_ANALYTICS_CHANNEL_SIZE", MaxChannelCapacity); ok {
		cfg.ChannelSize = n
	}
	if n, ok := positiveEnvInt("KORA_ANALYTICS_MAX_EVENT_BYTES", 16<<20); ok {
		cfg.MaxEventBytes = n
	}
	return cfg
}

func positiveEnvInt(name string, limit int) (int, bool) {
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil || n <= 0 || n > limit {
		return 0, false
	}
	return n, true
}
