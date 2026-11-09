package print

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/go-pdf/fpdf"
)

// RenderPDF lays the text output out in a monospace font. fpdf is pure Go
// (no CGO), MIT-licensed, maintained, and ships the core Courier font so no
// font files are embedded.
func (f *Format) RenderPDF(data Data, opt Options) ([]byte, error) {
	txt, err := f.RenderText(data, opt)
	if err != nil {
		return nil, err
	}
	cols := Columns(f.Paper)
	const size, lineH = 9.0, 4.0
	// Courier advance is 0.6em; 1pt = 1/72in.
	charMM := size * 0.6 * 25.4 / 72
	width := float64(cols)*charMM + 10
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "mm", Size: fpdf.SizeType{Wd: width, Ht: 297}})
	if f.Paper == "a4" {
		pdf = fpdf.New("P", "mm", "A4", "")
	}
	pdf.SetMargins(5, 5, 5)
	pdf.SetAutoPageBreak(true, 5)
	pdf.SetFont("Courier", "", size)
	pdf.AddPage()
	for _, l := range strings.Split(strings.TrimRight(txt, "\n"), "\n") {
		pdf.CellFormat(0, lineH, latin1(pdf, l), "", 1, "L", false, 0, "")
	}
	var b bytes.Buffer
	if err := pdf.Output(&b); err != nil {
		return nil, fmt.Errorf("%s: %w", ErrRenderFailed, err)
	}
	return b.Bytes(), nil
}

func latin1(p *fpdf.Fpdf, s string) string { return p.UnicodeTranslatorFromDescriptor("")(s) }
