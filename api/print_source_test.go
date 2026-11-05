package api

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/print"
)

func TestSitePrintSource(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f, err := print.Load([]byte("name: a\nfor: {record: Sale}\npaper: a4\ntext: x\n"))
	if err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	cases := []struct {
		name string
		run  func(sitePrintSource, *gin.Context) error
		want error
	}{
		{"known format", func(s sitePrintSource, c *gin.Context) error { _, err := s.Format(c, "a"); return err }, nil},
		{"unknown format", func(s sitePrintSource, c *gin.Context) error { _, err := s.Format(c, "zz"); return err }, ErrPrintNotFound},
		{"unknown doctype", func(s sitePrintSource, c *gin.Context) error { _, err := s.Record(c, "Nope", "x"); return err }, ErrPrintNotFound},
		{"receipt unsupported", func(s sitePrintSource, c *gin.Context) error { _, _, err := s.Receipt(c, "r", "e"); return err }, ErrPrintNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Set("site_name", "s")
			c.Set("site_registry", reg)
			c.Set("site_runtime_services", SiteRuntimeService{PrintFormats: map[string]*print.Format{"a": f}})
			if err := tc.run(sitePrintSource{h: NewHandler(reg, nil)}, c); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}

func TestSitePrintSourceRecordDeniedWithoutExecution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Sale"})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("site_registry", reg)
	_, err := sitePrintSource{h: NewHandler(reg, nil)}.Record(c, "Sale", "S-1")
	if !errors.Is(err, ErrPrintForbidden) {
		t.Fatalf("got %v", err)
	}
}
