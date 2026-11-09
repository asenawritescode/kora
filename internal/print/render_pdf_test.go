package print

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestPDF(t *testing.T) {
	f, err := Load([]byte(strings.Replace(slip, "%s", "thermal_80", 1)))
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.RenderPDF(Data{Doc: map[string]any{"title": "Shop", "qty": 1, "total": 2}, Now: time.Unix(0, 0)}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, []byte("%PDF")) || !bytes.Contains(out, []byte("/Type /Page")) {
		t.Fatalf("not a pdf: %.20q", out)
	}
}

func TestHTMLEscapes(t *testing.T) {
	f, err := Load([]byte(strings.Replace(slip, "%s", "thermal_80", 1)))
	if err != nil {
		t.Fatal(err)
	}
	d := Data{Doc: map[string]any{"title": "<script>x</script>", "qty": 1, "total": 2}}
	out, err := f.RenderHTML(d, Options{})
	if err != nil || strings.Contains(out, "<script>") || !strings.HasPrefix(out, "<pre>") {
		t.Fatalf("pre: %v %q", err, out)
	}
	f.HTML = "<b>{{ .doc.title }}</b>{{ money .doc.total }}"
	out, err = f.RenderHTML(d, Options{Currency: "KES", Precision: 2})
	if err != nil || strings.Contains(out, "<script>") || !strings.Contains(out, "KES 2.00") {
		t.Fatalf("custom: %v %q", err, out)
	}
}
