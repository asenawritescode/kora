package orm

import (
	"testing"
	"time"

	"github.com/asenawritescode/kora/doctype"
)

func TestNormalizeScannedFieldValue(t *testing.T) {
	t.Parallel()

	value := time.Date(2026, time.October, 21, 6, 12, 13, 456789000, time.UTC)
	tests := []struct {
		name      string
		fieldType string
		want      string
	}{
		{name: "date", fieldType: "Date", want: "2026-10-21"},
		{name: "time", fieldType: "Time", want: "06:12:13.456789"},
		{name: "datetime", fieldType: "Datetime", want: "2026-10-21T06:12:13.456789Z"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			field := &doctype.Field{Fieldtype: tt.fieldType}
			got, ok := normalizeScannedFieldValue(field, value).(string)
			if !ok {
				t.Fatalf("normalized value type = %T, want string", normalizeScannedFieldValue(field, value))
			}
			if got != tt.want {
				t.Fatalf("normalized value = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeScannedNumericFieldValue(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		fieldType string
		value     []byte
		want      any
	}{
		{fieldType: "Int", value: []byte("42"), want: int64(42)},
		{fieldType: "Float", value: []byte("12.5"), want: float64(12.5)},
		{fieldType: "Currency", value: []byte("500.000000000"), want: float64(500)},
		{fieldType: "Percent", value: []byte("7.25"), want: float64(7.25)},
		{fieldType: "Data", value: []byte("00123"), want: "00123"},
	} {
		t.Run(test.fieldType, func(t *testing.T) {
			t.Parallel()
			got := normalizeScannedFieldValue(&doctype.Field{Fieldtype: test.fieldType}, test.value)
			if got != test.want {
				t.Fatalf("normalizeScannedFieldValue() = %#v (%T), want %#v (%T)", got, got, test.want, test.want)
			}
		})
	}
}
