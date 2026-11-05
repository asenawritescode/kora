package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/print"
)

// Errors a PrintSource returns for the handler to map to HTTP statuses.
var (
	ErrPrintNotFound  = errors.New("print: not found")
	ErrPrintForbidden = errors.New("print: forbidden")
)

// PrintSource supplies print formats and permission-checked data.
// Record and Receipt must enforce read permission / receipt visibility.
type PrintSource interface {
	Format(c *gin.Context, name string) (*print.Format, error)
	Record(c *gin.Context, doctype, name string) (map[string]any, error)
	Receipt(c *gin.Context, ref, executionID string) (receipt, doc map[string]any, err error)
	Tenant(c *gin.Context) (map[string]any, print.Options)
}

// NewPrintHandler serves GET /api/v1/print/:format_name (as=text|html|pdf).
func NewPrintHandler(src PrintSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		if src == nil {
			notFoundError(c, "print.not_configured", "Print is not configured", nil)
			return
		}
		as := c.DefaultQuery("as", "text")
		if as != "text" && as != "html" && as != "pdf" {
			badRequestError(c, "print.format_unsupported", "as must be text, html or pdf", nil)
			return
		}
		f, err := src.Format(c, c.Param("format_name"))
		if err != nil {
			printError(c, err)
			return
		}
		tenant, opt := src.Tenant(c)
		data := print.Data{Tenant: tenant, Now: time.Now()}
		switch {
		case f.For.Record != "":
			if q := c.Query("record"); q != f.For.Record || c.Query("name") == "" {
				badRequestError(c, "print.target_mismatch", "record and name must match the format", nil)
				return
			}
			data.Doc, err = src.Record(c, f.For.Record, c.Query("name"))
		default:
			ref, id := c.Query("operation_ref"), c.Query("execution_id")
			if ref != f.For.Operation || id == "" {
				badRequestError(c, "print.target_mismatch", "operation_ref and execution_id must match the format", nil)
				return
			}
			data.Receipt, data.Doc, err = src.Receipt(c, ref, id)
		}
		if err != nil {
			printError(c, err)
			return
		}
		var out []byte
		ctype := "text/plain; charset=utf-8"
		switch as {
		case "html":
			var h string
			h, err = f.RenderHTML(data, opt)
			out, ctype = []byte(h), "text/html; charset=utf-8"
		case "pdf":
			out, err = f.RenderPDF(data, opt)
			ctype = "application/pdf"
		default:
			var t string
			t, err = f.RenderText(data, opt)
			out = []byte(t)
		}
		if err != nil {
			writeError(c, http.StatusUnprocessableEntity, "print.render_failed", "Print render failed", nil)
			return
		}
		c.Data(http.StatusOK, ctype, out)
	}
}

func printError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrPrintForbidden):
		writeError(c, http.StatusForbidden, "print.forbidden", "Not permitted", nil)
	case errors.Is(err, ErrPrintNotFound):
		notFoundError(c, "print.not_found", "Not found", nil)
	default:
		writeError(c, http.StatusInternalServerError, "print.failed", "Print failed", nil)
	}
}
