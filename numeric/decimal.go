// Package numeric holds exact decimal values for Float, Currency and Percent
// fields. Values travel through documents as json.Number so storage and JSON
// keep their exact decimal text instead of a binary float approximation.
package numeric

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"
)

// IsDecimalFieldtype reports whether a DocType field type stores an exact decimal.
func IsDecimalFieldtype(fieldtype string) bool {
	switch fieldtype {
	case "Float", "Currency", "Percent":
		return true
	}
	return false
}

// Parse converts a document, request or driver value into an exact decimal.
// float64 inputs use their shortest round-trip text, so a JSON number such as
// 0.1 becomes exactly 0.1 rather than its binary expansion.
func Parse(v any) (decimal.Decimal, bool) {
	switch n := v.(type) {
	case nil:
		return decimal.Decimal{}, false
	case decimal.Decimal:
		return n, true
	case json.Number:
		return parseText(string(n))
	case string:
		return parseText(n)
	case []byte:
		return parseText(string(n))
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return decimal.Decimal{}, false
		}
		return parseText(strconv.FormatFloat(n, 'f', -1, 64))
	case float32:
		return Parse(float64(n))
	case int:
		return decimal.NewFromInt(int64(n)), true
	case int32:
		return decimal.NewFromInt(int64(n)), true
	case int64:
		return decimal.NewFromInt(n), true
	case uint64:
		return decimal.NewFromUint64(n), true
	}
	return decimal.Decimal{}, false
}

func parseText(text string) (decimal.Decimal, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return decimal.Decimal{}, false
	}
	d, err := decimal.NewFromString(text)
	if err != nil {
		return decimal.Decimal{}, false
	}
	return d, true
}

// Number renders d as canonical JSON number text without trailing zeros.
func Number(d decimal.Decimal) json.Number {
	return json.Number(d.String())
}

// Normalize returns the canonical json.Number for v, or an error when v is not
// a decimal. Empty strings normalize to nil so optional fields stay NULL.
func Normalize(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
		return nil, nil
	}
	d, ok := Parse(v)
	if !ok {
		return nil, fmt.Errorf("%v is not a decimal number", v)
	}
	return Number(d), nil
}

// Float64 converts v to float64 for consumers that only need an approximate
// value, such as analytics or scripting runtimes without decimal support.
func Float64(v any) (float64, bool) {
	d, ok := Parse(v)
	if !ok {
		return 0, false
	}
	f, _ := d.Float64()
	return f, true
}
