package api

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/internal/doctype"
)

func importRequest(t *testing.T, h *Handler, spec, csv string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("spec", spec)
	fw, _ := w.CreateFormFile("file", "x.csv")
	_, _ = fw.Write([]byte(csv))
	_ = w.Close()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/import", &buf)
	c.Request.Header.Set("Content-Type", w.FormDataContentType())
	h.HandleImport(c)
	return rec
}

func TestHandleImportRejections(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Item", Fields: []doctype.Field{{Fieldname: "qty", Fieldtype: "Int"}}})
	reg.Register(&doctype.DocType{Name: "Owned", ModuleOwnerID: "mod", ModuleOwnershipMode: "commands_only"})
	reg.Register(&doctype.DocType{Name: "Row", IsChildTable: true})
	h := &Handler{Registry: reg}
	tests := []struct {
		name, spec string
		status     int
		code       string
	}{
		{"unsupported key", `{"import_id":"a","operation_ref":"x"}`, 400, "import.unsupported"},
		{"key_fields", `{"import_id":"a","key_fields":["a"]}`, 400, "import.unsupported"},
		{"owned", `{"import_id":"a","target":{"doctype":"Owned"},"mapping":{"A":"b"}}`, 400, "import.target_owned"},
		{"child", `{"import_id":"a","target":{"doctype":"Row"},"mapping":{"A":"b"}}`, 400, "import.target_invalid"},
		{"no execution", `{"import_id":"a","target":{"doctype":"Item"},"mapping":{"A":"qty"}}`, 403, "permission.denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := importRequest(t, h, tt.spec, "A\n1\n")
			if rec.Code != tt.status || !strings.Contains(rec.Body.String(), tt.code) {
				t.Fatalf("got %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestParseCSVTableBOMAndSemicolon(t *testing.T) {
	h, rows, err := parseCSVTable([]byte("\xef\xbb\xbfsku;qty\nA;1\nB;2\n"))
	if err != nil || h[0] != "sku" || len(rows) != 2 || rows[1][1] != "2" {
		t.Fatalf("%v %v %v", h, rows, err)
	}
}

func TestParseImportValue(t *testing.T) {
	tests := []struct {
		ft, in string
		want   any
		bad    bool
	}{
		{"Currency", "12.50", "12.50", false},
		{"Currency", "1,5x", nil, true},
		{"Int", "1.5", nil, true},
		{"Date", "31/12/2025", "2025-12-31", false},
		{"Date", "2025-12-31", "2025-12-31", false},
		{"Date", "nope", nil, true},
		{"Check", "Yes", true, false},
		{"Check", "maybe", nil, true},
		{"Data", " hi ", "hi", false},
	}
	for _, tt := range tests {
		got, err := parseImportValue(&doctype.Field{Fieldtype: tt.ft}, tt.in)
		if (err != nil) != tt.bad || got != tt.want {
			t.Errorf("%s %q: got %v %v", tt.ft, tt.in, got, err)
		}
	}
}
