package tenanttime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/asenawritescode/kora/internal/db"
)

// Keys in _kora_site_setting for the tenant clock.
const (
	KeyTimeZone          = "time_zone"
	KeyBusinessDayCutoff = "business_day_cutoff"
)

const maxCutoff = 6 * time.Hour

// ParseZone returns the location for an IANA name. "Local" and "" are rejected
// because they depend on the server, not the tenant.
func ParseZone(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "Local" {
		return nil, fmt.Errorf("time zone %q is not an IANA zone", name)
	}
	return time.LoadLocation(name)
}

// ParseCutoff reads an HH:MM business-day cutoff between 00:00 and 06:00.
func ParseCutoff(s string) (time.Duration, error) {
	parsed, err := time.Parse("15:04", s)
	if err != nil || parsed.Format("15:04") != s {
		return 0, fmt.Errorf("cutoff %q is not HH:MM", s)
	}
	cutoff := time.Duration(parsed.Hour())*time.Hour + time.Duration(parsed.Minute())*time.Minute
	if cutoff > maxCutoff {
		return 0, fmt.Errorf("cutoff %q is after 06:00", s)
	}
	return cutoff, nil
}

// FormatCutoff renders a cutoff as HH:MM, the form ParseCutoff accepts.
func FormatCutoff(cutoff time.Duration) string {
	return fmt.Sprintf("%02d:%02d", int(cutoff.Hours()), int(cutoff.Minutes())%60)
}

// Querier reads one row. *sql.DB and *sql.Tx both satisfy it.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Load reads the site's stored zone and cutoff. Unset keys take Default; a
// stored value that no longer parses is an error.
func Load(ctx context.Context, q Querier, dialect db.QueryDialect, site string) (Clock, error) {
	zone, err := storedSetting(ctx, q, dialect, site, KeyTimeZone)
	if err != nil {
		return Clock{}, err
	}
	cutoff, err := storedSetting(ctx, q, dialect, site, KeyBusinessDayCutoff)
	if err != nil {
		return Clock{}, err
	}
	clock := Default()
	if zone != "" {
		if clock.Location, err = ParseZone(zone); err != nil {
			return Clock{}, fmt.Errorf("stored %s: %w", KeyTimeZone, err)
		}
	}
	if cutoff != "" {
		if clock.Cutoff, err = ParseCutoff(cutoff); err != nil {
			return Clock{}, fmt.Errorf("stored %s: %w", KeyBusinessDayCutoff, err)
		}
	}
	return clock, nil
}

// storedSetting returns "" when the key is unset.
func storedSetting(ctx context.Context, q Querier, dialect db.QueryDialect, site, key string) (string, error) {
	var value string
	err := q.QueryRowContext(ctx, db.Rebind(dialect, "SELECT setting_value FROM _kora_site_setting WHERE site = ? AND setting_key = ?"), site, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read site setting %s: %w", key, err)
	}
	return value, nil
}
