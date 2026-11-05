package site

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
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

// resolvePrintOperations rewrites each print format's for.operation, given as
// a command full name (namespace.name), to the published operation ref of the
// merged pack tree. A value containing "~" must already be a published ref.
func resolvePrintOperations(snapshot *doctype.ConfigSnapshot) error {
	published := map[string]bool{}
	for _, m := range snapshot.Modules {
		for _, p := range m.PublishedOperations {
			published[contract.PublishedOperationRef(m.ID, p.Capability, p.Operation)] = true
		}
	}
	byName := map[string]string{}
	for _, raw := range snapshot.Commands {
		cmd, err := kernel.ParseCommandResourceDraftJSON(raw)
		if err != nil {
			return fmt.Errorf("pack.command_invalid: %w", err)
		}
		if cmd.ModuleOwnerID == "" {
			continue
		}
		ref := contract.PublishedOperationRef(contract.ModuleID(cmd.ModuleOwnerID), cmd.Capability, cmd.Operation)
		if published[ref] {
			byName[cmd.Namespace+"."+cmd.Name] = ref
		}
	}
	for i, raw := range snapshot.PrintFormats {
		f, err := print.Decode(raw)
		if err != nil {
			return fmt.Errorf("decode print format: %w", err)
		}
		op := f.For.Operation
		if op == "" {
			continue
		}
		ref := op
		if !strings.Contains(op, "~") {
			ref = byName[op]
		}
		if !published[ref] {
			return fmt.Errorf("print.operation_unpublished: print format %s: operation %q is not a published operation", f.Name, op)
		}
		f.For.Operation = ref
		if snapshot.PrintFormats[i], err = print.Encode(f); err != nil {
			return fmt.Errorf("encoding print format %s: %w", f.Name, err)
		}
	}
	return nil
}
