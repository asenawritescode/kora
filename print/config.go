// Package print defines print-format templates and renders them to text.
package print

import (
	"errors"
	"fmt"
	"strings"
	"text/template"
	"text/template/parse"

	"gopkg.in/yaml.v3"
)

// Limits from spec D3.
const (
	MaxTemplateBytes = 32 * 1024
	MaxOutputBytes   = 256 * 1024
)

// ErrTemplateInvalid is the load-time error code.
const ErrTemplateInvalid = "print.template_invalid"

// ErrRenderFailed is the render-time error code.
const ErrRenderFailed = "print.render_failed"

// Target says what a format prints: a record or an operation receipt.
type Target struct {
	Record    string `yaml:"record,omitempty"`
	Operation string `yaml:"operation,omitempty"`
}

// Format is one print_formats/*.yaml document.
type Format struct {
	Name  string `yaml:"name"`
	For   Target `yaml:"for"`
	Paper string `yaml:"paper"`
	Text  string `yaml:"text"`
	HTML  string `yaml:"html,omitempty"`

	tmpl *template.Template
}

// Columns returns the text width for a paper value (0 for a4/unknown).
func Columns(paper string) int {
	switch paper {
	case "thermal_58":
		return 32
	case "thermal_80":
		return 48
	case "a4":
		return 80
	}
	return 0
}

// Load parses strict YAML and validates the format.
func Load(data []byte) (*Format, error) {
	var f Format
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, invalid("yaml: %v", err)
	}
	if f.Name == "" {
		return nil, invalid("name required")
	}
	if (f.For.Record == "") == (f.For.Operation == "") {
		return nil, invalid("for needs exactly one of record or operation")
	}
	if Columns(f.Paper) == 0 {
		return nil, invalid("unknown paper %q", f.Paper)
	}
	if len(f.Text) > MaxTemplateBytes || len(f.HTML) > MaxTemplateBytes {
		return nil, invalid("template exceeds %d bytes", MaxTemplateBytes)
	}
	t, err := template.New(f.Name).Funcs(funcMap(Columns(f.Paper), Options{})).Parse(f.Text)
	if err != nil {
		return nil, invalid("%v", err)
	}
	if name := forbiddenIdent(t); name != "" {
		return nil, invalid("function %q not allowed", name)
	}
	f.tmpl = t
	return &f, nil
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%s: %w", ErrTemplateInvalid, errors.New(fmt.Sprintf(format, a...)))
}

// allowedBuiltins are the text/template builtins kept; call, html, js,
// urlquery and slice are rejected.
var allowedBuiltins = map[string]bool{
	"and": true, "or": true, "not": true, "eq": true, "ne": true, "lt": true,
	"le": true, "gt": true, "ge": true, "len": true, "index": true,
	"print": true, "printf": true, "println": true,
}

func forbiddenIdent(t *template.Template) string {
	for _, tt := range t.Templates() {
		if tt.Tree == nil {
			continue
		}
		if n := walkIdent(tt.Tree.Root, funcMap(1, Options{})); n != "" {
			return n
		}
	}
	return ""
}

func walkIdent(n parse.Node, own template.FuncMap) string {
	var bad string
	var visit func(parse.Node)
	visit = func(n parse.Node) {
		if n == nil || bad != "" {
			return
		}
		switch v := n.(type) {
		case *parse.ListNode:
			if v == nil {
				return
			}
			for _, c := range v.Nodes {
				visit(c)
			}
		case *parse.ActionNode:
			visit(v.Pipe)
		case *parse.PipeNode:
			if v == nil {
				return
			}
			for _, c := range v.Cmds {
				visit(c)
			}
		case *parse.CommandNode:
			for _, a := range v.Args {
				visit(a)
			}
		case *parse.IdentifierNode:
			if _, ok := own[v.Ident]; !ok && !allowedBuiltins[v.Ident] {
				bad = v.Ident
			}
		case *parse.IfNode:
			visit(v.Pipe)
			visit(v.List)
			visit(v.ElseList)
		case *parse.RangeNode:
			visit(v.Pipe)
			visit(v.List)
			visit(v.ElseList)
		case *parse.WithNode:
			visit(v.Pipe)
			visit(v.List)
			visit(v.ElseList)
		}
	}
	visit(n)
	return bad
}
