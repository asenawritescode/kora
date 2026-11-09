package script

import "encoding/json"

// jsValue converts exact decimals to float64 before values enter the JS
// runtime. Goja would expose json.Number as a string, so `a + b` would
// concatenate instead of add.
func jsValue(v any) any {
	switch typed := v.(type) {
	case json.Number:
		if f, err := typed.Float64(); err == nil {
			return f
		}
		return string(typed)
	case map[string]any:
		if typed == nil {
			return typed
		}
		out := make(map[string]any, len(typed))
		for k, item := range typed {
			out[k] = jsValue(item)
		}
		return out
	case []map[string]any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = jsValue(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = jsValue(item)
		}
		return out
	}
	return v
}
