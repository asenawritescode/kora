package metrics

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/asenawritescode/kora/internal/doctype"
	goyaml "github.com/goccy/go-yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

var (
	metricKeys  = yamlKeys(Metric{})
	measureKeys = yamlKeys(Measure{})
	filterKeys  = yamlKeys(Filter{})
)

// ParseDirectory reads the metric documents of a pack's metrics/ directory. A missing directory has none.
func ParseDirectory(dir string) ([]Metric, error) {
	return readYAMLDir(dir, ParseYAML)
}

// readYAMLDir parses every YAML file of dir with parse. A missing directory has no documents.
func readYAMLDir[T any](dir string, parse func([]byte, string) (T, error)) ([]T, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var docs []T
	for _, e := range entries {
		if ext := filepath.Ext(e.Name()); e.IsDir() || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		doc, err := parse(body, path)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

// ParseYAML decodes one metric document; source names it in errors.
func ParseYAML(body []byte, source string) (Metric, error) {
	var m Metric
	if err := checkKeys(body, metricKeys, nestedKeyProblems); err != nil {
		return m, fmt.Errorf("%s: %w", source, err)
	}
	if err := goyaml.UnmarshalWithOptions(body, &m, goyaml.DisallowUnknownField()); err != nil {
		return m, fmt.Errorf("%s: yaml: %w", source, err)
	}
	return m, nil
}

// checkKeys reports unknown keys of a document, with line numbers and did-you-mean hints.
// nested checks the keys of the mappings under each top-level key.
func checkKeys(body []byte, known map[string]bool, nested func(key string, v *yamlv3.Node) []string) error {
	var root yamlv3.Node
	if err := yamlv3.Unmarshal(body, &root); err != nil {
		return fmt.Errorf("yaml: %w", err)
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yamlv3.MappingNode {
		return nil // not a mapping: the decoder reports it
	}
	top := root.Content[0]
	problems := unknownKeys(top, known)
	for i := 0; i+1 < len(top.Content); i += 2 {
		problems = append(problems, nested(top.Content[i].Value, top.Content[i+1])...)
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
}

// nestedKeyProblems checks the measure mapping, or each filter of the filters list.
func nestedKeyProblems(key string, v *yamlv3.Node) []string {
	switch {
	case key == "measure" && v.Kind == yamlv3.MappingNode:
		return unknownKeys(v, measureKeys)
	case key == "filters" && v.Kind == yamlv3.SequenceNode:
		var problems []string
		for _, item := range v.Content {
			if item.Kind == yamlv3.MappingNode {
				problems = append(problems, unknownKeys(item, filterKeys)...)
			}
		}
		return problems
	}
	return nil
}

// unknownKeys lists the keys of a mapping node that known does not contain.
func unknownKeys(node *yamlv3.Node, known map[string]bool) []string {
	var problems []string
	for i := 0; i+1 < len(node.Content); i += 2 {
		k := node.Content[i]
		if !known[k.Value] {
			problems = append(problems, strings.TrimSpace(fmt.Sprintf("line %d: unknown key %q %s", k.Line, k.Value, doctype.KeySuggestion(k.Value, known))))
		}
	}
	return problems
}

// yamlKeys lists the YAML keys a struct decodes from.
func yamlKeys(v any) map[string]bool {
	keys := map[string]bool{}
	t := reflect.TypeOf(v)
	for i := range t.NumField() {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ","); name != "" {
			keys[name] = true
		}
	}
	return keys
}

// LoadPack reads a pack's metrics/ and dashboards/ directories and returns both as the JSON
// documents a config snapshot carries. Metrics and dashboards are checked together against the pack's record types.
func LoadPack(packDir string, doctypes []*doctype.DocType) ([]json.RawMessage, []json.RawMessage, error) {
	defs, err := ParseDirectory(filepath.Join(packDir, "metrics"))
	if err != nil {
		return nil, nil, fmt.Errorf("parsing metrics: %w", err)
	}
	dashboards, err := ParseDashboardDirectory(filepath.Join(packDir, "dashboards"))
	if err != nil {
		return nil, nil, fmt.Errorf("parsing dashboards: %w", err)
	}
	return EncodePack(defs, dashboards, doctypes)
}

// EncodePack checks metric and dashboard definitions against the record types of a pack and returns
// both as the JSON documents a config snapshot carries.
func EncodePack(defs []Metric, dashboards []Dashboard, doctypes []*doctype.DocType) ([]json.RawMessage, []json.RawMessage, error) {
	metricDocs, err := EncodeValidated(defs, doctypes)
	if err != nil {
		return nil, nil, err
	}
	if err = ValidateDashboards(dashboards, defs, doctypes); err != nil {
		return nil, nil, fmt.Errorf("validating dashboards: %w", err)
	}
	dashboardDocs, err := encodeDocs(dashboards)
	if err != nil {
		return nil, nil, err
	}
	return metricDocs, dashboardDocs, nil
}

// EncodeValidated checks definitions against the record types of a pack and returns them
// as the JSON documents a config snapshot carries.
func EncodeValidated(defs []Metric, doctypes []*doctype.DocType) ([]json.RawMessage, error) {
	if err := Validate(defs, doctypes); err != nil {
		return nil, fmt.Errorf("validating metrics: %w", err)
	}
	return encodeDocs(defs)
}

// encodeDocs marshals each definition to the JSON document a config snapshot carries.
func encodeDocs[T Metric | Dashboard](defs []T) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(defs))
	for _, d := range defs {
		raw, err := json.Marshal(d)
		if err != nil {
			return nil, fmt.Errorf("encoding definition: %w", err)
		}
		out = append(out, raw)
	}
	return out, nil
}
