package db

import (
	"fmt"
	"time"
)

// Timestamp scans native SQL timestamp values and the UTC text values returned
// by SQLite/LibSQL timestamp expressions.
type Timestamp struct{ time.Time }

func (stamp *Timestamp) Scan(value any) error {
	if stamp == nil {
		return fmt.Errorf("scan database timestamp into nil receiver")
	}
	switch value := value.(type) {
	case time.Time:
		stamp.Time = value
		return nil
	case string:
		return stamp.parse(value)
	case []byte:
		return stamp.parse(string(value))
	case nil:
		return fmt.Errorf("database timestamp is NULL")
	default:
		return fmt.Errorf("unsupported database timestamp type %T", value)
	}
}

func (stamp *Timestamp) parse(value string) error {
	layouts := []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05", "2006-01-02 15:04:05"}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			stamp.Time = parsed
			return nil
		}
		if parsed, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			stamp.Time = parsed
			return nil
		}
	}
	return fmt.Errorf("parse database timestamp %q: unsupported format", value)
}
