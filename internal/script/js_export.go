package script

import (
	"encoding/json"
	"fmt"
	"strings"
)

// decodeJSValue turns the JSON text of a script's return value into Go values the
// way goja's Export does: integers that fit int64 become int64, other numbers float64.
func decodeJSValue(jsonText string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(jsonText))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("script: decode result: %w", err)
	}
	return exportNumbers(v)
}

// exportNumbers replaces every json.Number in v, recursively, with int64 or float64.
func exportNumbers(v any) (any, error) {
	switch typed := v.(type) {
	case json.Number:
		return exportNumber(typed)
	case map[string]any:
		return typed, exportMap(typed)
	case []any:
		return typed, exportSlice(typed)
	}
	return v, nil
}

// exportNumber returns n as int64 when it is an integer that fits, else float64.
func exportNumber(n json.Number) (any, error) {
	if i, err := n.Int64(); err == nil {
		return i, nil
	}
	f, err := n.Float64()
	if err != nil {
		return nil, fmt.Errorf("script: number out of range: %w", err)
	}
	return f, nil
}

// exportMap converts the values of m in place.
func exportMap(m map[string]any) error {
	for k, item := range m {
		out, err := exportNumbers(item)
		if err != nil {
			return err
		}
		m[k] = out
	}
	return nil
}

// exportSlice converts the elements of s in place.
func exportSlice(s []any) error {
	for i, item := range s {
		out, err := exportNumbers(item)
		if err != nil {
			return err
		}
		s[i] = out
	}
	return nil
}
