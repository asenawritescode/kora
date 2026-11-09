package print

import (
	"os"
	"strings"
	"testing"
	"time"
)

const slip = `name: slip
for: {record: Purchase Request}
paper: %s
text: |
  {{ center .doc.title }}
  {{ line }}
  {{ left 10 "Tea" }}{{ right 8 (num .doc.qty) }}
  {{ right 12 (money .doc.total) }}
`

func render(t *testing.T, paper string, opt Options) string {
	t.Helper()
	f, err := Load([]byte(strings.Replace(slip, "%s", paper, 1)))
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.RenderText(Data{Doc: map[string]any{"title": "Shop", "qty": 1200, "total": 1250.75}, Now: time.Unix(0, 0)}, opt)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGolden(t *testing.T) {
	for _, paper := range []string{"thermal_58", "thermal_80"} {
		got := render(t, paper, Options{Currency: "KES", Precision: 2})
		want, err := os.ReadFile("testdata/" + paper + ".golden")
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s mismatch:\n%q\nwant\n%q", paper, got, want)
		}
		for _, l := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
			if len(l) > Columns(paper) {
				t.Errorf("line over width: %q", l)
			}
		}
	}
}

func TestMoneyPrecision(t *testing.T) {
	if got := render(t, "thermal_58", Options{Currency: "KES", Precision: 0}); !strings.Contains(got, "KES 1,251") {
		t.Errorf("0dp: %q", got)
	}
	if got := render(t, "thermal_58", Options{Currency: "KES", Precision: 2}); !strings.Contains(got, "KES 1,250.75") {
		t.Errorf("2dp: %q", got)
	}
}

func TestTruncate(t *testing.T) {
	f, _ := Load([]byte("name: a\nfor: {record: X}\npaper: thermal_58\ntext: '{{ left 5 \"abcdefgh\" }}|'\n"))
	out, _ := f.RenderText(Data{}, Options{})
	if out != "abcde|" {
		t.Errorf("got %q", out)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]string{
		"unknown func": "name: a\nfor: {record: X}\npaper: a4\ntext: '{{ call .x }}'\n",
		"bad paper":    "name: a\nfor: {record: X}\npaper: letter\ntext: x\n",
		"unknown key":  "name: a\nfor: {record: X}\npaper: a4\ntext: x\nbogus: 1\n",
		"no target":    "name: a\npaper: a4\ntext: x\n",
		"bad syntax":   "name: a\nfor: {record: X}\npaper: a4\ntext: '{{ if }}'\n",
		"too large":    "name: a\nfor: {record: X}\npaper: a4\ntext: '" + strings.Repeat("x", MaxTemplateBytes+1) + "'\n",
	}
	for name, y := range cases {
		_, err := Load([]byte(y))
		if err == nil || !strings.Contains(err.Error(), ErrTemplateInvalid) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
}

func TestOutputLimit(t *testing.T) {
	f, _ := Load([]byte("name: a\nfor: {record: X}\npaper: a4\ntext: '{{ range $i := .doc.n }}{{ repeat 1000 \"x\" }}{{ end }}'\n"))
	n := make([]int, 300)
	_, err := f.RenderText(Data{Doc: map[string]any{"n": n}}, Options{})
	if err == nil || !strings.Contains(err.Error(), ErrRenderFailed) {
		t.Errorf("err=%v", err)
	}
}
