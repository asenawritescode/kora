package api

import (
	"fmt"
	"time"
)

func parseRealtimeTimestamp(value any) (time.Time, error) {
	switch value := value.(type) {
	case time.Time:
		return value, nil
	case string:
		return parseRealtimeTimestampText(value)
	case []byte:
		return parseRealtimeTimestampText(string(value))
	default:
		return time.Time{}, fmt.Errorf("unsupported value type %T", value)
	}
}

func parseRealtimeTimestampText(value string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp format")
}
