package print

import (
	"strings"
	"testing"
)

func TestLoadQueries(t *testing.T) {
	head := "name: a\nfor: {record: Sale}\npaper: a4\n"
	cases := []struct {
		name, queries, wantErr string
	}{
		{"good", "queries: {by_method: {query: pos.core/pm, input: {t: doc.name, r: receipt.ref, l: lit}}}\n", ""},
		{"too many", "queries: {a: {query: m/q}, b: {query: m/q}, c: {query: m/q}, d: {query: m/q}, e: {query: m/q}}\n", "at most 4"},
		{"bad key", "queries: {Bad: {query: m/q}}\n", "lower-case"},
		{"bad ref", "queries: {a: {query: noslash}}\n", "<module>/<name>"},
		{"bad input", "queries: {a: {query: m/q, input: {t: 'doc.'}}}\n", "bad input ref"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]byte(head + tc.queries + "text: x\n"))
			if (tc.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err %v want %q", err, tc.wantErr)
			}
		})
	}
}
