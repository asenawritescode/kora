package script

import (
	"reflect"
	"testing"
)

func TestDecodeJSValue(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    any
		wantErr bool
	}{
		{name: "integer is int64", in: `42`, want: int64(42)},
		{name: "negative integer is int64", in: `-7`, want: int64(-7)},
		{name: "fraction is float64", in: `1.5`, want: 1.5},
		{name: "exponent form is float64", in: `1e3`, want: float64(1000)},
		{name: "string", in: `"abc"`, want: "abc"},
		{name: "boolean", in: `true`, want: true},
		{name: "null", in: `null`, want: nil},
		{name: "nested values", in: `{"a":[1,"x",2.5],"b":{"c":false}}`, want: map[string]any{
			"a": []any{int64(1), "x", 2.5},
			"b": map[string]any{"c": false},
		}},
		{name: "invalid JSON", in: `{"a":`, wantErr: true},
		{name: "number beyond float64", in: `1e400`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeJSValue(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("decodeJSValue(%q) error = nil, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeJSValue(%q): %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("decodeJSValue(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}
