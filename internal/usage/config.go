package usage

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/asenawritescode/kora/internal/envcompat"
)

const (
	urlEnv          = "KORA_CLOUD_USAGE_URL"
	intervalEnv     = "KORA_CLOUD_USAGE_INTERVAL"
	defaultInterval = 6 * time.Hour
	minInterval     = 10 * time.Minute
)

// Config says where and how often usage snapshots are sent.
type Config struct {
	URL      string
	Token    string
	Interval time.Duration
}

// LoadConfig reads the usage settings through lookup (os.Getenv in the
// server). It returns nil when KORA_CLOUD_USAGE_URL is unset, which turns
// reporting off.
func LoadConfig(lookup func(string) string) (*Config, error) {
	url := strings.TrimSpace(lookup(urlEnv))
	if url == "" {
		return nil, nil
	}
	cfg := &Config{URL: url, Token: strings.TrimSpace(lookup(envcompat.IntegrationTokenEnv)), Interval: defaultInterval}
	if cfg.Token == "" {
		return nil, fmt.Errorf("%s needs %s", urlEnv, envcompat.IntegrationTokenEnv)
	}
	if raw := strings.TrimSpace(lookup(intervalEnv)); raw != "" {
		interval, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", intervalEnv, err)
		}
		if interval < minInterval {
			return nil, errors.New(intervalEnv + " must be at least 10m")
		}
		cfg.Interval = interval
	}
	return cfg, nil
}
