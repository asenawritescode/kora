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
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var defs []Metric
	for _, e := range entries {
		if ext := filepath.Ext(e.Name()); e.IsDir() || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		m, err := parse(body)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		defs = append(defs, m)
	}
	return defs, nil
}

func parse(body []byte) (Metric, error) {
	var m Metric
	if err := checkKeys(body); err != nil {
		return m, err
	}
	if err := goyaml.UnmarshalWithOptions(body, &m, goyaml.DisallowUnknownField()); err != nil {
		return m, fmt.Errorf("yaml: %w", err)
	}
	return m, nil
}

// checkKeys reports unknown keys of a metric document, with line numbers and did-you-mean hints.
func checkKeys(body []byte) error {
	var root yamlv3.Node
	if err := yamlv3.Unmarshal(body, &root); err != nil {
		return fmt.Errorf("yaml: %w", err)
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yamlv3.MappingNode {
		return nil // not a mapping: the decoder reports it
	}
	top := root.Content[0]
	problems := unknownKeys(top, metricKeys)
	for i := 0; i+1 < len(top.Content); i += 2 {
		problems = append(problems, nestedKeyProblems(top.Content[i].Value, top.Content[i+1])...)
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

// Load parses the metrics/ directory of a pack, checks the definitions against the pack's
// record types and returns them as the JSON documents a config snapshot carries.
func Load(dir string, doctypes []*doctype.DocType) ([]json.RawMessage, error) {
	defs, err := ParseDirectory(dir)
	if err != nil {
		return nil, fmt.Errorf("parsing metrics: %w", err)
	}
	if err := Validate(defs, doctypes); err != nil {
		return nil, fmt.Errorf("validating metrics: %w", err)
	}
	out := make([]json.RawMessage, 0, len(defs))
	for _, m := range defs {
		raw, err := json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("encoding metric %s: %w", m.Name, err)
		}
		out = append(out, raw)
	}
	return out, nil
}
