package site

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/print"
)

// loadPrintFormats reads print_formats/*.yaml sorted by file name and encodes
// each for the config snapshot. A missing directory yields no formats.
func loadPrintFormats(dir string, doctypes []*doctype.DocType, queries map[string]bool) ([]json.RawMessage, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading print formats: %w", err)
	}
	known := map[string]bool{}
	for _, dt := range doctypes {
		known[dt.Name] = true
	}
	var names []string
	for _, e := range entries {
		if ext := strings.ToLower(filepath.Ext(e.Name())); !e.IsDir() && (ext == ".yaml" || ext == ".yml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	seen := map[string]bool{}
	var out []json.RawMessage
	for _, n := range names {
		body, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", n, err)
		}
		f, err := print.Load(body)
		if err != nil {
			return nil, fmt.Errorf("print format %s: %w", n, err)
		}
		if seen[f.Name] {
			return nil, fmt.Errorf("print format %s: %s: %w", n, print.ErrTemplateInvalid, errors.New("duplicate name "+f.Name))
		}
		seen[f.Name] = true
		if f.For.Record != "" && !known[f.For.Record] {
			return nil, fmt.Errorf("print format %s: %s: %w", n, print.ErrTemplateInvalid, errors.New("unknown record "+f.For.Record))
		}
		for key, q := range f.Queries {
			if !queries[q.Query] {
				return nil, fmt.Errorf("print format %s: %s: %w", n, print.ErrTemplateInvalid, errors.New("query "+key+": unknown query "+q.Query))
			}
		}
		raw, err := print.Encode(f)
		if err != nil {
			return nil, fmt.Errorf("encoding print format %s: %w", n, err)
		}
		out = append(out, raw)
	}
	return out, nil
}
