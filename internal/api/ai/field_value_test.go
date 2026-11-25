package ai

import (
	"encoding/json"
	"testing"
	"time"
)

func TestFieldValueSame(t *testing.T) {
	day := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	dayInZone := day.In(time.FixedZone("UTC+3", 3*60*60))
	for _, tc := range []struct {
		name   string
		a, b   any
		number bool
		want   bool
	}{
		{name: "int and float64 of one number", a: 700, b: 700.0, want: true},
		{name: "int and decimal text in a numeric field", a: 700, b: "700.000000000", number: true, want: true},
		{name: "int and bytes of decimal text in a numeric field", a: 700, b: []byte("700"), number: true, want: true},
		{name: "int and json number", a: int64(1), b: json.Number("1.000000"), want: true},
		{name: "float and decimal text in a numeric field", a: 2.5, b: "2.50", number: true, want: true},
		{name: "int and text of one number in a numeric field", a: 1, b: "1.000000", number: true, want: true},
		{name: "different numbers", a: 700, b: 701, want: false},
		{name: "phone numbers in a text field differ by a leading zero", a: "0722695333", b: "722695333", want: false},
		{name: "numeric text in a text field compares as text", a: "700", b: "700.00", want: false},
		{name: "decimal texts in a numeric field compare by value", a: "700", b: "700.00", number: true, want: true},
		{name: "numbers and text that is not a number", a: 700, b: "Open", want: false},
		{name: "different words", a: "Open", b: "Paid", want: false},
		{name: "same words", a: "Paid", b: "Paid", want: true},
		{name: "nil and empty text", a: nil, b: "", want: true},
		{name: "nil and text", a: nil, b: "x", want: false},
		{name: "nil and number", a: nil, b: 0, want: false},
		{name: "same instant in two zones", a: day, b: dayInZone, want: true},
		{name: "different instants", a: day, b: day.AddDate(0, 0, 1), want: false},
		{name: "date as stored and as returned", a: "2026-11-25", b: "2026-11-25", want: true},
		{name: "different dates", a: "2026-11-25", b: "2026-11-26", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := fieldValue{v: tc.a, number: tc.number}, fieldValue{v: tc.b, number: tc.number}
			if got := a.same(b); got != tc.want {
				t.Fatalf("same(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			if got := b.same(a); got != tc.want {
				t.Fatalf("same(%v, %v) = %v, want %v (reversed)", tc.b, tc.a, got, tc.want)
			}
		})
	}
}
