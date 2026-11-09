package api

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/internal/print"
)

// Errors a PrintSource returns for the handler to map to HTTP statuses.
var (
	ErrPrintNotFound  = errors.New("print: not found")
	ErrPrintForbidden = errors.New("print: forbidden")
	// ErrPrintRenderFailed marks a data source failure that maps to 422.
	ErrPrintRenderFailed = errors.New("print: render failed")
)

// PrintSource supplies print formats and permission-checked data.
// Record and Receipt must enforce read permission / receipt visibility.
type PrintSource interface {
	Format(c *gin.Context, name string) (*print.Format, error)
	Record(c *gin.Context, doctype, name string) (map[string]any, error)
	Receipt(c *gin.Context, ref, executionID string) (receipt, doc map[string]any, err error)
	Tenant(c *gin.Context) (map[string]any, print.Options)
	// Formats lists the tenant's formats; Authorize applies the render route's
	// read rules to a format's targets without needing a record.
	Formats(c *gin.Context) []*print.Format
	Authorize(c *gin.Context, f *print.Format) error
	// Query runs a published query as the viewer; it returns ErrPrintForbidden
	// on a denied grant and ErrPrintNotFound for an unknown query.
	Query(c *gin.Context, module, name string, input map[string]any) ([]map[string]any, error)
}

type printFormatMeta struct {
	Name    string       `json:"name"`
	Target  print.Target `json:"target"`
	Paper   string       `json:"paper"`
	Columns int          `json:"columns"`
	Formats []string     `json:"formats"`
}

func printMeta(f *print.Format) printFormatMeta {
	formats := []string{"text", "pdf"}
	if f.HTML != "" {
		formats = []string{"text", "html", "pdf"}
	}
	return printFormatMeta{Name: f.Name, Target: f.For, Paper: f.Paper, Columns: print.Columns(f.Paper), Formats: formats}
}

// NewPrintListHandler serves GET /api/v1/print-formats, optionally filtered by
// ?record= or ?operation_ref=. Only formats the caller may print are listed.
func NewPrintListHandler(src PrintSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		if src == nil {
			notFoundError(c, "print.not_configured", "Print is not configured", nil)
			return
		}
		rec, op := c.Query("record"), c.Query("operation_ref")
		out := []printFormatMeta{}
		for _, f := range src.Formats(c) {
			if (rec != "" && f.For.Record != rec) || (op != "" && f.For.Operation != op) {
				continue
			}
			if src.Authorize(c, f) == nil {
				out = append(out, printMeta(f))
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		c.JSON(http.StatusOK, gin.H{"data": out, "meta": gin.H{"total": len(out)}})
	}
}

// NewPrintMetaHandler serves GET /api/v1/print-formats/:format_name.
func NewPrintMetaHandler(src PrintSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		if src == nil {
			notFoundError(c, "print.not_configured", "Print is not configured", nil)
			return
		}
		f, err := src.Format(c, c.Param("format_name"))
		if err == nil {
			err = src.Authorize(c, f)
		}
		if err != nil {
			printError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": printMeta(f)})
	}
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
			if err == nil && f.For.Doc != "" {
				name, _ := data.Receipt["reference"].(string)
				if name == "" {
					writeError(c, http.StatusUnprocessableEntity, "print.render_failed", "Print render failed", nil)
					return
				}
				data.Doc, err = src.Record(c, f.For.Doc, name)
			}
		}
		if err == nil && len(f.Queries) > 0 {
			data.Queries = map[string][]map[string]any{}
			for key, spec := range f.Queries {
				module, name, _ := spec.Ref()
				var rows []map[string]any
				if rows, err = src.Query(c, module, name, spec.ResolveInput(data.Doc, data.Receipt)); err != nil {
					break
				}
				data.Queries[key] = rows
			}
		}
		if err != nil {
			// A denied or failed query renders nothing: no partial rows.
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
	case errors.Is(err, ErrPrintRenderFailed):
		writeError(c, http.StatusUnprocessableEntity, print.ErrRenderFailed, "Print render failed", nil)
	default:
		writeError(c, http.StatusInternalServerError, "print.failed", "Print failed", nil)
	}
}
