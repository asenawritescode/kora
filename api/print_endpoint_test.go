package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/print"
)

type fakePrint struct {
	f    *print.Format
	err  error
	last string
}

func (p *fakePrint) Format(*gin.Context, string) (*print.Format, error) { return p.f, nil }
func (p *fakePrint) Record(_ *gin.Context, dt, n string) (map[string]any, error) {
	p.last = dt + "/" + n
	return map[string]any{"title": "Tea"}, p.err
}
func (p *fakePrint) Receipt(*gin.Context, string, string) (map[string]any, map[string]any, error) {
	return map[string]any{"reference": "R1"}, map[string]any{"title": "Doc"}, p.err
}
func (p *fakePrint) Tenant(*gin.Context) (map[string]any, print.Options) {
	return map[string]any{"name": "Shop"}, print.Options{Currency: "KES", Precision: 2}
}

func TestPrintEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec, _ := print.Load([]byte("name: a\nfor: {record: Sale}\npaper: thermal_58\ntext: \"{{ .tenant.name }} {{ .doc.title }}\"\n"))
	op, _ := print.Load([]byte("name: b\nfor: {operation: pos~pos.sales~record_sale}\npaper: thermal_58\ntext: \"{{ .receipt.reference }} {{ .doc.title }}\"\n"))
	withDoc, _ := print.Load([]byte("name: c\nfor: {operation: pos~pos.sales~record_sale, doc: Sale}\npaper: thermal_58\ntext: \"{{ .receipt.reference }} {{ .doc.title }}\"\n"))
	cases := []struct {
		name   string
		f      *print.Format
		err    error
		url    string
		status int
		body   string
	}{
		{"record", rec, nil, "/p/a?record=Sale&name=S-1", 200, "Shop Tea"},
		{"operation", op, nil, "/p/b?operation_ref=pos~pos.sales~record_sale&execution_id=e1", 200, "R1 Doc"},
		{"operation doc", withDoc, nil, "/p/c?operation_ref=pos~pos.sales~record_sale&execution_id=e1", 200, "R1 Tea"},
		{"forbidden", rec, ErrPrintForbidden, "/p/a?record=Sale&name=S-1", 403, "print.forbidden"},
		{"not found", rec, ErrPrintNotFound, "/p/a?record=Sale&name=S-1", 404, "print.not_found"},
		{"target mismatch", rec, nil, "/p/a?record=Other&name=S-1", 400, "print.target_mismatch"},
		{"bad format", rec, nil, "/p/a?record=Sale&name=S-1&as=docx", 400, "print.format_unsupported"},
		{"html record", rec, nil, "/p/a?record=Sale&name=S-1&as=html", 200, "<pre>Shop Tea</pre>"},
		{"html receipt", op, nil, "/p/b?operation_ref=pos~pos.sales~record_sale&execution_id=e1&as=html", 200, "<pre>R1 Doc</pre>"},
		{"pdf record", rec, nil, "/p/a?record=Sale&name=S-1&as=pdf", 200, "%PDF-"},
		{"pdf receipt", op, nil, "/p/b?operation_ref=pos~pos.sales~record_sale&execution_id=e1&as=pdf", 200, "%PDF-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			r.GET("/p/:format_name", NewPrintHandler(&fakePrint{f: tc.f, err: tc.err}))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.url, nil))
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.body) {
				t.Fatalf("got %d %q", w.Code, w.Body.String())
			}
		})
	}
	r := gin.New()
	r.GET("/p/:format_name", NewPrintHandler(nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/p/a", nil))
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
}
