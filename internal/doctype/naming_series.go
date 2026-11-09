package doctype

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// NamingSeries is a parsed document naming template such as
// "INV-.YYYY.-.#####" or "LOAN-.{branch}.-.####". Parts are separated by dots:
// date tokens (YYYY, YY, MM, DD) expand from the creation time, {field} expands
// from the document, a final run of '#' sets the zero-padded counter width,
// and anything else is literal. The text before the counter is the series
// key, so each distinct year or field value numbers independently.
type NamingSeries struct {
	parts  []seriesPart
	digits int
}

type seriesPart struct {
	literal string
	date    string
	field   string
}

var seriesFieldValue = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// ParseNamingSeries validates a naming template.
func ParseNamingSeries(template string) (NamingSeries, error) {
	template = strings.TrimSpace(template)
	if template == "" {
		return NamingSeries{}, fmt.Errorf("naming series is empty")
	}
	raw := strings.Split(template, ".")
	last := raw[len(raw)-1]
	if last == "" || strings.Trim(last, "#") != "" {
		return NamingSeries{}, fmt.Errorf("naming series %q must end with a counter such as .#####", template)
	}
	if len(last) > 12 {
		return NamingSeries{}, fmt.Errorf("naming series %q counter is wider than 12 digits", template)
	}
	series := NamingSeries{digits: len(last)}
	for _, part := range raw[:len(raw)-1] {
		switch {
		case part == "":
			continue
		case strings.Contains(part, "#"):
			return NamingSeries{}, fmt.Errorf("naming series %q may contain only one counter, at the end", template)
		case part == "YYYY" || part == "YY" || part == "MM" || part == "DD":
			series.parts = append(series.parts, seriesPart{date: part})
		case strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}"):
			field := strings.TrimSpace(part[1 : len(part)-1])
			if field == "" {
				return NamingSeries{}, fmt.Errorf("naming series %q has an empty field token", template)
			}
			series.parts = append(series.parts, seriesPart{field: field})
		default:
			series.parts = append(series.parts, seriesPart{literal: part})
		}
	}
	return series, nil
}

// Fields returns the document fields the series reads.
func (s NamingSeries) Fields() []string {
	var out []string
	for _, part := range s.parts {
		if part.field != "" {
			out = append(out, part.field)
		}
	}
	return out
}

// Prefix expands every part before the counter. Field values must be short
// identifiers so a series key never carries arbitrary text.
func (s NamingSeries) Prefix(values map[string]any, at time.Time) (string, error) {
	var b strings.Builder
	for _, part := range s.parts {
		switch {
		case part.date == "YYYY":
			b.WriteString(at.Format("2006"))
		case part.date == "YY":
			b.WriteString(at.Format("06"))
		case part.date == "MM":
			b.WriteString(at.Format("01"))
		case part.date == "DD":
			b.WriteString(at.Format("02"))
		case part.field != "":
			value := strings.TrimSpace(fmt.Sprint(values[part.field]))
			if values[part.field] == nil || !seriesFieldValue.MatchString(value) {
				return "", fmt.Errorf("naming series field %q must be 1-40 letters, digits, '-' or '_'", part.field)
			}
			b.WriteString(value)
		default:
			b.WriteString(part.literal)
		}
	}
	return b.String(), nil
}

// Format appends the zero-padded counter to an expanded prefix.
func (s NamingSeries) Format(prefix string, number int64) string {
	return fmt.Sprintf("%s%0*d", prefix, s.digits, number)
}

// CounterFromName returns the counter a stored name carries for prefix, or
// false when the name does not belong to that series.
func CounterFromName(prefix, name string) (int64, bool) {
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	digits := strings.TrimPrefix(name, prefix)
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	return n, err == nil
}
