package site

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/print"
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

func posMergedSnapshot(t *testing.T) *doctype.ConfigSnapshot {
	t.Helper()
	dirs, err := ResolvePackTree(filepath.Join("..", "..", "config", "pos"))
	if err != nil {
		t.Fatal(err)
	}
	merged, names := &doctype.ConfigSnapshot{}, map[string]bool{}
	for _, d := range dirs {
		s, err := parsePackSnapshot(d)
		if err != nil {
			t.Fatal(err)
		}
		if err := mergePackSnapshot(merged, s, names); err != nil {
			t.Fatal(err)
		}
	}
	return merged
}

func TestResolvePrintOperations(t *testing.T) {
	const sale, closeTill, closeSession = "pos.core~pos.checkout~record_sale", "pos.core~pos.drawer~close_till", "pos.core~pos.drawer~close_session"
	merged := posMergedSnapshot(t)
	if err := resolvePrintOperations(merged); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, raw := range merged.PrintFormats {
		f, _ := print.Decode(raw)
		got[f.Name] = f.For.Operation
	}
	if got["sale_receipt"] != sale || got["z_report"] != closeTill || got["z_report_close_session"] != closeSession {
		t.Fatalf("stored refs: %v", got)
	}
	if err := resolvePrintOperations(merged); err != nil { // exact ref stays accepted
		t.Fatalf("idempotent: %v", err)
	}
	for _, op := range []string{"pos.nope", "pos.core~pos.checkout~nope", "record_sale"} {
		bad := posMergedSnapshot(t)
		f, _ := print.Decode(bad.PrintFormats[0])
		f.For.Operation = op
		f.For.Record = ""
		bad.PrintFormats[0], _ = print.Encode(f)
		if err := resolvePrintOperations(bad); err == nil || !strings.Contains(err.Error(), "print.operation_unpublished") {
			t.Errorf("%s: err %v", op, err)
		}
	}
}
