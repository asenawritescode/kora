package print

import (
	"bytes"
	"fmt"
	htmltemplate "html/template"
)

// RenderHTML renders the html template (auto-escaped). With no html
// template it wraps the text output in <pre>.
func (f *Format) RenderHTML(data Data, opt Options) (string, error) {
	if f.HTML == "" {
		txt, err := f.RenderText(data, opt)
		if err != nil {
			return "", err
		}
		return "<pre>" + htmltemplate.HTMLEscapeString(txt) + "</pre>", nil
	}
	t, err := htmltemplate.New(f.Name).Funcs(funcMap(Columns(f.Paper), opt)).Parse(f.HTML)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ErrRenderFailed, err)
	}
	var b bytes.Buffer
	if err := t.Execute(&limitWriter{w: &b}, data.tmplValue()); err != nil {
		return "", fmt.Errorf("%s: %w", ErrRenderFailed, err)
	}
	return b.String(), nil
}
