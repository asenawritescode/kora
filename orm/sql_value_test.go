package orm

import (
	"testing"
	"time"

	"github.com/asenawritescode/kora/doctype"
)

func TestNormalizeSQLValueParsesDatetimeFromJSON(t *testing.T) {
	got, err := normalizeSQLValue(doctype.Field{Fieldtype: "Datetime"}, "2026-10-01T20:55:57.689361542Z")
	if err != nil {
		t.Fatalf("normalize datetime: %v", err)
	}
	parsed, ok := got.(time.Time)
	if !ok {
		t.Fatalf("normalized datetime type = %T, want time.Time", got)
	}
	want := time.Date(2026, 10, 1, 20, 55, 57, 689361542, time.UTC)
	if !parsed.Equal(want) {
		t.Fatalf("normalized datetime = %s, want %s", parsed, want)
	}
	if _, err := normalizeSQLValue(doctype.Field{Fieldtype: "Datetime"}, "not-a-date"); err == nil {
		t.Fatal("invalid datetime string was accepted")
	}
}
