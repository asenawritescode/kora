package doctype

import (
	"strings"
	"testing"
	"time"
)

func TestNamingSeriesExpandsTemplates(t *testing.T) {
	at := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		template string
		values   map[string]any
		prefix   string
		name     string
	}{
		{"INV-.YYYY.-.#####", nil, "INV-2026-", "INV-2026-00007"},
		{"LOAN-.{branch}.-.YY.MM.-.###", map[string]any{"branch": "WESTLANDS"}, "LOAN-WESTLANDS-2611-", "LOAN-WESTLANDS-2611-007"},
		{"T.DD.####", nil, "T05", "T050007"},
	}
	for _, tc := range cases {
		series, err := ParseNamingSeries(tc.template)
		if err != nil {
			t.Fatalf("%s: %v", tc.template, err)
		}
		prefix, err := series.Prefix(tc.values, at)
		if err != nil {
			t.Fatalf("%s: %v", tc.template, err)
		}
		if prefix != tc.prefix {
			t.Fatalf("%s prefix = %q, want %q", tc.template, prefix, tc.prefix)
		}
		if got := series.Format(prefix, 7); got != tc.name {
			t.Fatalf("%s name = %q, want %q", tc.template, got, tc.name)
		}
		if n, ok := CounterFromName(prefix, tc.name); !ok || n != 7 {
			t.Fatalf("%s counter = %d %v", tc.template, n, ok)
		}
	}
}

func TestNamingSeriesRejectsInvalidTemplatesAndValues(t *testing.T) {
	for template, want := range map[string]string{
		"":                "empty",
		"INV-.YYYY":       "must end with a counter",
		"INV-.##.-.###":   "only one counter",
		"INV-.{}.-.###":   "empty field token",
		"X.#############": "wider than 12",
	} {
		if _, err := ParseNamingSeries(template); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q err = %v, want %q", template, err, want)
		}
	}
	series, _ := ParseNamingSeries("LOAN-.{branch}.-.###")
	for _, value := range []any{nil, "", "two words", "x'; DROP TABLE y; --", strings.Repeat("a", 41)} {
		if _, err := series.Prefix(map[string]any{"branch": value}, time.Now()); err == nil {
			t.Fatalf("value %q was accepted", value)
		}
	}
	if _, ok := CounterFromName("INV-2026-", "INV-2026-12a"); ok {
		t.Fatal("non-numeric suffix was accepted")
	}
}

func TestDocTypeValidatesNamingSeries(t *testing.T) {
	base := func() *DocType {
		return &DocType{Name: "Loan", Module: "Library", Fields: []Field{{Fieldname: "branch", Fieldtype: "Data", Label: "Branch"}}}
	}
	ok := base()
	ok.NamingSeries = "LOAN-.{branch}.-.####"
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	missing := base()
	missing.NamingSeries = "LOAN-.{desk}.-.####"
	if err := missing.Validate(); err == nil || !strings.Contains(err.Error(), "desk") {
		t.Fatalf("missing field err = %v", err)
	}
	child := base()
	child.IsChildTable = true
	child.NamingSeries = "LOAN-.####"
	if err := child.Validate(); err == nil {
		t.Fatal("child table series was accepted")
	}
	parsed, err := FromSExpr(ToSExpr(&ConfigSnapshot{DocTypes: []*DocType{ok}}))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.DocTypes[0].NamingSeries != ok.NamingSeries {
		t.Fatalf("naming series lost in snapshot: %q", parsed.DocTypes[0].NamingSeries)
	}
}
