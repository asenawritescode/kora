// Package query parses and validates declarative published queries (D2).
package query

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/doctype"
	"gopkg.in/yaml.v3"
)

// ErrInvalidDefinition is the code reported for load-time failures.
const ErrInvalidDefinition = "query.invalid_definition"

// Filter is one entry of the AND list: a leaf (Field/Op/Value) or an OR group (Any).
type Filter struct {
	Field    string   `yaml:"field"`
	Op       string   `yaml:"op"`
	Value    any      `yaml:"value"`
	Any      []Filter `yaml:"any"`
	Optional bool     `yaml:"optional"`
}

// InputSpec describes one call input.
type InputSpec struct {
	Type      string   `yaml:"type"`
	MaxLength int      `yaml:"max_length"`
	Enum      []string `yaml:"enum"`
}

// Sort is one ordering key.
type Sort struct {
	Field string `yaml:"field"`
	Dir   string `yaml:"dir"`
}

// Definition is a query shipped in queries/*.yaml.
type Definition struct {
	Name            string               `yaml:"name"`
	ModuleOwnerID   string               `yaml:"module_owner_id"`
	Capability      string               `yaml:"capability"`
	Operation       string               `yaml:"operation"`
	Source          string               `yaml:"source"`
	Permission      string               `yaml:"permission"`
	Scope           string               `yaml:"scope"`
	OwnerField      string               `yaml:"owner_field"`
	Input           map[string]InputSpec `yaml:"input"`
	Filters         []Filter             `yaml:"filters"`
	Fields          []string             `yaml:"fields"`
	Sort            []Sort               `yaml:"sort"`
	Limit           int                  `yaml:"limit"`
	OfflineEligible bool                 `yaml:"offline_eligible"`
	Presentation    map[string]string    `yaml:"presentation"`
}

// CatalogRef is owner~capability~operation.
func (d *Definition) CatalogRef() string {
	return d.ModuleOwnerID + "~" + d.Capability + "~" + d.Operation
}

const (
	defaultLimit = 50
	maxLimit     = 200
)

var (
	operators   = map[string]bool{"eq": true, "ne": true, "lt": true, "lte": true, "gt": true, "gte": true, "in": true, "contains": true, "starts_with": true, "is_null": true}
	inputTypes  = map[string]bool{"string": true, "int": true, "number": true, "bool": true}
	systemCols  = map[string]bool{"name": true, "owner": true, "creation": true, "modified": true, "modified_by": true, "doc_status": true, "idx": true}
	bannedTypes = map[string]bool{"Password": true, "JSON": true, "Table": true}
)

// Parse decodes strictly (unknown keys rejected) and validates against reg.
func Parse(raw []byte, reg *doctype.Registry) (*Definition, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	var d Definition
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("%s: %w", ErrInvalidDefinition, err)
	}
	if err := d.validate(reg); err != nil {
		return nil, fmt.Errorf("%s: query %q: %w", ErrInvalidDefinition, d.Name, err)
	}
	return &d, nil
}

// LoadDir parses every yaml file in dir; a missing dir yields none.
func LoadDir(dir string, reg *doctype.Registry) ([]*Definition, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("query dir %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && (strings.HasSuffix(n, ".yaml") || strings.HasSuffix(n, ".yml")) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var defs []*Definition
	seen := map[string]string{}
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", n, err)
		}
		d, err := Parse(raw, reg)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		if prev, dup := seen[d.CatalogRef()]; dup {
			return nil, fmt.Errorf("%s: %s: duplicate catalog ref %s (also %s)", n, ErrInvalidDefinition, d.CatalogRef(), prev)
		}
		seen[d.CatalogRef()] = n
		defs = append(defs, d)
	}
	return defs, nil
}

func (d *Definition) validate(reg *doctype.Registry) error {
	for k, v := range map[string]string{"name": d.Name, "module_owner_id": d.ModuleOwnerID, "capability": d.Capability, "operation": d.Operation, "source": d.Source, "permission": d.Permission} {
		if v == "" {
			return fmt.Errorf("missing %s", k)
		}
	}
	if d.Scope != "tenant" && d.Scope != "legal_entity" {
		return fmt.Errorf("scope must be tenant or legal_entity, got %q", d.Scope)
	}
	if d.Limit == 0 {
		d.Limit = defaultLimit
	}
	if d.Limit < 0 || d.Limit > maxLimit {
		return fmt.Errorf("limit must be 1..%d, got %d", maxLimit, d.Limit)
	}
	dt := reg.Get(d.Source)
	if dt == nil {
		return fmt.Errorf("source doctype %q not found", d.Source)
	}
	check := func(ctx, f string) error {
		if systemCols[f] {
			return nil
		}
		fld := dt.GetField(f)
		if fld == nil {
			return fmt.Errorf("%s: field %q not on %s", ctx, f, d.Source)
		}
		if bannedTypes[fld.Fieldtype] {
			return fmt.Errorf("%s: field %q has unselectable type %s", ctx, f, fld.Fieldtype)
		}
		return nil
	}
	if len(d.Fields) == 0 {
		return fmt.Errorf("fields must not be empty")
	}
	for _, f := range d.Fields {
		if err := check("fields", f); err != nil {
			return err
		}
	}
	if d.OwnerField != "" {
		if err := check("owner_field", d.OwnerField); err != nil {
			return err
		}
	}
	for _, s := range d.Sort {
		if err := check("sort", s.Field); err != nil {
			return err
		}
		if s.Dir != "" && s.Dir != "asc" && s.Dir != "desc" {
			return fmt.Errorf("sort: dir must be asc or desc, got %q", s.Dir)
		}
	}
	for name, in := range d.Input {
		if !inputTypes[in.Type] {
			return fmt.Errorf("input %q: unsupported type %q", name, in.Type)
		}
	}
	for _, f := range d.Filters {
		if err := d.validateFilter(f, check, false); err != nil {
			return err
		}
	}
	return nil
}

func (d *Definition) validateFilter(f Filter, check func(string, string) error, nested bool) error {
	if len(f.Any) > 0 {
		if nested {
			return fmt.Errorf("filters: nested any groups are not supported")
		}
		if f.Field != "" || f.Op != "" {
			return fmt.Errorf("filters: any group cannot also set field/op")
		}
		for _, c := range f.Any {
			if err := d.validateFilter(c, check, true); err != nil {
				return err
			}
		}
		return nil
	}
	if !operators[f.Op] {
		return fmt.Errorf("filters: unknown operator %q", f.Op)
	}
	if err := check("filters", f.Field); err != nil {
		return err
	}
	if s, ok := f.Value.(string); ok && strings.HasPrefix(s, "$input.") {
		if _, ok := d.Input[strings.TrimPrefix(s, "$input.")]; !ok {
			return fmt.Errorf("filters: %s references undeclared input", s)
		}
	}
	return nil
}
