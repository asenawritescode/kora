package print

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"
)

const renderTimeout = 2 * time.Second

// Options carry tenant-level formatting inputs for helpers.
type Options struct {
	Currency  string // e.g. KES
	Precision int    // decimals for money
}

// Data is the template root.
type Data struct {
	Doc     map[string]any
	Receipt map[string]any
	Tenant  map[string]any
	Now     time.Time
	// Queries holds the rows of each format query, keyed by its queries: key.
	Queries map[string][]map[string]any
}

func (d Data) tmplValue() map[string]any {
	return map[string]any{"doc": d.Doc, "receipt": d.Receipt, "tenant": d.Tenant, "now": d.Now, "queries": d.Queries}
}

// RenderText renders the text template at the paper width.
func (f *Format) RenderText(data Data, opt Options) (string, error) {
	cols := Columns(f.Paper)
	t, err := template.New(f.Name).Funcs(funcMap(cols, opt)).Parse(f.Text)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ErrRenderFailed, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), renderTimeout)
	defer cancel()
	type res struct {
		out string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		var b bytes.Buffer
		err := t.Execute(&limitWriter{w: &b}, data.tmplValue())
		ch <- res{b.String(), err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return "", fmt.Errorf("%s: %w", ErrRenderFailed, r.err)
		}
		return r.out, nil
	case <-ctx.Done():
		return "", fmt.Errorf("%s: render timeout", ErrRenderFailed)
	}
}

type limitWriter struct {
	w *bytes.Buffer
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.w.Len()+len(p) > MaxOutputBytes {
		return 0, fmt.Errorf("output exceeds %d bytes", MaxOutputBytes)
	}
	return l.w.Write(p)
}

func fit(n int, s string) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) > n {
		return string([]rune(s)[:n])
	}
	return s
}

func funcMap(cols int, opt Options) template.FuncMap {
	return template.FuncMap{
		"left": func(n int, v any) string {
			s := fit(n, fmt.Sprint(v))
			return s + strings.Repeat(" ", n-utf8.RuneCountInString(s))
		},
		"right": func(n int, v any) string {
			s := fit(n, fmt.Sprint(v))
			return strings.Repeat(" ", n-utf8.RuneCountInString(s)) + s
		},
		"center": func(v any) string {
			s := fit(cols, fmt.Sprint(v))
			pad := cols - utf8.RuneCountInString(s)
			return strings.Repeat(" ", pad/2) + s
		},
		"line":  func() string { return strings.Repeat("-", cols) },
		"upper": func(v any) string { return strings.ToUpper(fmt.Sprint(v)) },
		"repeat": func(n int, s string) string {
			if n < 0 || n*len(s) > MaxOutputBytes {
				return ""
			}
			return strings.Repeat(s, n)
		},
		"num": func(v any) string { return formatNum(v, -1) },
		"money": func(v any) string {
			s := formatNum(v, opt.Precision)
			if opt.Currency != "" {
				return opt.Currency + " " + s
			}
			return s
		},
		"date": func(v any) string {
			if t, ok := v.(time.Time); ok {
				return t.Format("2006-01-02 15:04")
			}
			return fmt.Sprint(v)
		},
	}
}

// formatNum formats with thousands separators; prec<0 keeps natural decimals.
func formatNum(v any, prec int) string {
	f, err := strconv.ParseFloat(fmt.Sprint(v), 64)
	if err != nil {
		return fmt.Sprint(v)
	}
	s := strconv.FormatFloat(f, 'f', prec, 64)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	ip, fp, _ := strings.Cut(s, ".")
	var b strings.Builder
	for i, c := range ip {
		if i > 0 && (len(ip)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := b.String()
	if fp != "" {
		out += "." + fp
	}
	if neg {
		out = "-" + out
	}
	return out
}
