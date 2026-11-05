package numeric

import (
	"encoding/json"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want any
		err  bool
	}{
		{"float keeps shortest text", 0.1, json.Number("0.1"), false},
		{"float sum noise is preserved as typed", 0.30000000000000004, json.Number("0.30000000000000004"), false},
		{"driver bytes trim trailing zeros", []byte("1250.500000000"), json.Number("1250.5"), false},
		{"numeric string", "19.99", json.Number("19.99"), false},
		{"json number", json.Number("1000000000000.123456789"), json.Number("1000000000000.123456789"), false},
		{"integer", int64(42), json.Number("42"), false},
		{"three decimal currency", "0.125", json.Number("0.125"), false},
		{"empty string is null", "", nil, false},
		{"nil is null", nil, nil, false},
		{"not a number", "abc", nil, true},
		{"bool rejected", true, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Normalize(tt.in)
			if (err != nil) != tt.err {
				t.Fatalf("err = %v, want error %v", err, tt.err)
			}
			if got != tt.want {
				t.Fatalf("Normalize(%v) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestNumberMarshalsAsExactJSONNumber(t *testing.T) {
	d, _ := Parse("1000000000000.123456789")
	out, err := json.Marshal(map[string]any{"amount": Number(d)})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"amount":1000000000000.123456789}` {
		t.Fatalf("marshal = %s", out)
	}
}

func TestIsDecimalFieldtype(t *testing.T) {
	for _, ft := range []string{"Float", "Currency", "Percent"} {
		if !IsDecimalFieldtype(ft) {
			t.Fatalf("%s should be decimal", ft)
		}
	}
	if IsDecimalFieldtype("Int") || IsDecimalFieldtype("Data") {
		t.Fatal("Int and Data are not decimal")
	}
}
