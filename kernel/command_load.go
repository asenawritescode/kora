package kernel

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LoadCommandDir parses every *.yaml file in dir (sorted by filename for
// deterministic registration order) into a fresh CommandRegistry. Parsing is
// strict: any invalid definition aborts the whole load and returns the
// accumulated errors — configuration is never silently discarded.
// A missing directory returns an empty registry and no error, so deployments
// without command definitions need no special-casing.
func LoadCommandDir(dir string) (*CommandRegistry, error) {
	reg := NewCommandRegistry()
	defs, err := ParseCommandDir(dir)
	if err != nil {
		return reg, err
	}
	for _, def := range defs {
		if err := reg.Register(def); err != nil {
			return reg, fmt.Errorf("command definitions invalid: %w", err)
		}
	}
	return reg, nil
}

// ParseCommandDir parses every *.yaml file in dir, sorted by filename. A
// missing directory yields no definitions and no error.
func ParseCommandDir(dir string) ([]*CommandResource, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("command dir %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var errs []error
	defs := make([]*CommandResource, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("read %s: %w", path, err))
			continue
		}
		def, err := ParseCommandResource(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			continue
		}
		defs = append(defs, def)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("command definitions invalid (%d errors): %w", len(errs), errs[0])
	}
	return defs, nil
}
