package site

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/print"
)

func TestLoadPrintFormats(t *testing.T) {
	fmtYAML := func(name, rec string) string {
		return "name: " + name + "\nfor: {record: " + rec + "}\npaper: a4\ntext: x\n"
	}
	cases := []struct {
		name    string
		files   map[string]string
		want    int
		wantErr string
	}{
		{"none", nil, 0, ""},
		{"two sorted", map[string]string{"b.yaml": fmtYAML("b", "Sale"), "a.yaml": fmtYAML("a", "Sale")}, 2, ""},
		{"duplicate", map[string]string{"a.yaml": fmtYAML("a", "Sale"), "b.yaml": fmtYAML("a", "Sale")}, 0, "duplicate"},
		{"unknown record", map[string]string{"a.yaml": fmtYAML("a", "Nope")}, 0, "unknown record"},
		{"known query", map[string]string{"a.yaml": fmtYAML("a", "Sale") + "queries: {k: {query: pos/q}}\n"}, 1, ""},
		{"unknown query", map[string]string{"a.yaml": fmtYAML("a", "Sale") + "queries: {k: {query: pos/nope}}\n"}, 0, "unknown query"},
		{"invalid", map[string]string{"a.yaml": "name: a\n"}, 0, print.ErrTemplateInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "print_formats")
			if tc.files != nil {
				_ = os.MkdirAll(dir, 0o755)
			}
			for n, body := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := loadPrintFormats(dir, []*doctype.DocType{{Name: "Sale"}}, map[string]bool{"pos/q": true})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err %v want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || len(got) != tc.want {
				t.Fatalf("got %d, %v", len(got), err)
			}
			if tc.want == 2 {
				if f, _ := print.Decode(got[0]); f.Name != "a" {
					t.Fatalf("not sorted: %s", f.Name)
				}
			}
		})
	}
}
